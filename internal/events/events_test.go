package events

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/clientip"
)

// stream opens chain's event stream as device guid and returns its event
// names and data, skipping comments.
func stream(t *testing.T, server *httptest.Server, token, guid string) (<-chan string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"?cache_guid="+guid, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the reader below closes it
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: HTTP %d", resp.StatusCode)
	}
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		var event string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				lines <- event + " " + strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return lines, cancel
}

func next(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case line := <-lines:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return ""
	}
}

func TestHub(t *testing.T) {
	hub := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 2, 100)
	server := httptest.NewServer(withChain(hub))
	defer server.Close()

	chainID := newChain(t)
	token := chainID

	a, stopA := stream(t, server, token, "a")
	defer stopA()
	b, stopB := stream(t, server, token, "b")
	defer stopB()
	for _, lines := range []<-chan string{a, b} {
		if got := next(t, lines); got != "ready {}" {
			t.Fatalf("first event %q, want ready", got)
		}
	}

	// The committing device is not told about its own change.
	hub.Notify(context.Background(), chainID, "a", []int32{32904})
	if got := next(t, b); got != `changed {"types":[32904]}` {
		t.Fatalf("b got %q", got)
	}
	hub.Notify(context.Background(), "other chain", "", []int32{1})
	hub.resyncAll()
	if got := next(t, a); got != "resync {}" {
		t.Fatalf("a got %q, want only the resync", got)
	}

	// The chain already has its two streams.
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third stream: HTTP %d, want 429", resp.StatusCode)
	}
}

func TestSubscriberMergesPendingEvents(t *testing.T) {
	sub := &subscriber{wake: make(chan struct{}, 1)}
	sub.add([]int32{1, 2}, false)
	sub.add([]int32{2, 3}, false)
	dataTypes, resync := sub.take()
	if len(dataTypes) != 3 || resync {
		t.Fatalf("took %v resync %v, want [1 2 3]", dataTypes, resync)
	}
	if dataTypes, _ := sub.take(); dataTypes != nil {
		t.Fatalf("took %v twice", dataTypes)
	}
}

// newChain returns a random chain ID; the tests send it as the bearer
// token, and withChain puts it in the context as account.ChainAccess would.
func newChain(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

func withChain(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chainID := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		next.ServeHTTP(w, r.WithContext(auth.WithChain(r.Context(), chainID)))
	})
}

func status(t *testing.T, server *httptest.Server, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestStreamsPerAddress(t *testing.T) {
	hub := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 10, 100)
	hub.MaxPerAddr = 2
	var resolver *clientip.Resolver
	server := httptest.NewServer(resolver.Middleware(withChain(hub)))
	defer server.Close()

	// Two chains from the same address.
	first := newChain(t)
	second := newChain(t)
	a, stopA := stream(t, server, first, "a")
	next(t, a)
	b, stopB := stream(t, server, second, "b")
	next(t, b)
	defer stopB()
	if got := status(t, server, second); got != http.StatusTooManyRequests {
		t.Fatalf("third stream from one address: HTTP %d, want 429", got)
	}
	stopA()
	for i := 0; hub.Streams() > 1; i++ {
		if i > 100 {
			t.Fatal("the closed stream stays")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c, stopC := stream(t, server, first, "c")
	defer stopC()
	next(t, c)
}

func TestClose(t *testing.T) {
	hub := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 10, 100)
	server := httptest.NewServer(withChain(hub))
	defer server.Close()
	token := newChain(t)
	lines, stop := stream(t, server, token, "a")
	defer stop()
	next(t, lines)

	hub.Close()
	select {
	case _, open := <-lines:
		if open {
			t.Fatal("got an event after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream stays open after Close")
	}
	if got := status(t, server, token); got != http.StatusServiceUnavailable {
		t.Fatalf("stream after Close: HTTP %d, want 503", got)
	}
}
