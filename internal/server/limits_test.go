package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/metrics"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	"github.com/VizzleTF/arcomium-sync/internal/store"
	"github.com/VizzleTF/arcomium-sync/internal/store/memory"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

func newToken(t *testing.T) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewToken(key, time.Now())
}

func baseConfig(st store.Store) Config {
	return Config{
		Service: protocol.New(st, nil, protocol.DefaultLimits),
		Events:  http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "stream") }),
		Health:  func(context.Context) error { return nil },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestSlowBodyDoesNotHoldSlots(t *testing.T) {
	c := baseConfig(memory.New())
	c.MaxConcurrent = 1
	c.BodyTimeout = 500 * time.Millisecond
	srv := httptest.NewServer(Handler(c))
	defer srv.Close()
	token := newToken(t)

	// A client sends its headers and then nothing.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /v2/command/ HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 100\r\n\r\n", token)
	time.Sleep(100 * time.Millisecond)

	// The only slot is free for others meanwhile.
	start := time.Now()
	resp, err := http.Post(srv.URL+"/v2/command/", "application/octet-stream", bytes.NewReader(getUpdates(t)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusOK {
		t.Fatalf("other request: HTTP %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/command/", bytes.NewReader(getUpdates(t)))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || time.Since(start) > c.BodyTimeout {
		t.Fatalf("other request: HTTP %d after %v", resp.StatusCode, time.Since(start))
	}

	// The slow client is cut off.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(status, "408") {
		t.Fatalf("slow client got %q, %v; want 408", status, err)
	}
}

func TestAddrLimit(t *testing.T) {
	c := baseConfig(memory.New())
	c.AddrLimiter = ratelimit.New(1)
	h := Handler(c)
	token := newToken(t)
	req := request{path: "/v2/command/", body: getUpdates(t), token: token}
	if rsp := response(t, do(h, req)); rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
		t.Fatalf("first request: %v", rsp.GetErrorCode())
	}
	// Another chain from the same address.
	req.token = newToken(t)
	if rsp := response(t, do(h, req)); rsp.GetErrorCode() != pb.SyncEnums_THROTTLED {
		t.Fatalf("second request: %v, want THROTTLED", rsp.GetErrorCode())
	}
	rec := httptest.NewRecorder()
	events := httptest.NewRequest(http.MethodGet, "/v2/events", nil)
	events.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rec, events)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("events: HTTP %d, want 429", rec.Code)
	}
}

func TestEventsNeedAChain(t *testing.T) {
	st := memory.New()
	c := baseConfig(st)
	c.ChainExists = func(ctx context.Context, chainID string) (bool, error) {
		_, ok, err := st.Chain(ctx, chainID)
		return ok, err
	}
	h := Handler(c)
	token := newToken(t)
	stream := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v2/events?cache_guid=g", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := stream(); got != http.StatusNotFound {
		t.Fatalf("stream before the first GetUpdates: HTTP %d, want 404", got)
	}
	do(h, request{path: "/v2/command/", body: getUpdates(t), token: token})
	if got := stream(); got != http.StatusOK {
		t.Fatalf("stream of an existing chain: HTTP %d", got)
	}
}

func TestProbes(t *testing.T) {
	ready := true
	var dbErr error
	c := baseConfig(memory.New())
	c.Health = func(context.Context) error { return dbErr }
	c.Ready = func() bool { return ready }
	h := Handler(c)
	get := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	for _, test := range []struct {
		ready                bool
		db                   error
		livez, readyz, healt int
	}{
		{true, nil, 200, 200, 200},
		{false, nil, 200, 503, 200},
		{true, errors.New("down"), 200, 503, 503},
	} {
		ready, dbErr = test.ready, test.db
		if got := [3]int{get("/livez"), get("/readyz"), get("/health")}; got != [3]int{test.livez, test.readyz, test.healt} {
			t.Errorf("ready %v, db %v: livez, readyz, health = %v", test.ready, test.db, got)
		}
	}
}

// cancelCheckingStore fails writes whose context ended.
type cancelCheckingStore struct{ *memory.Store }

func (s cancelCheckingStore) Create(ctx context.Context, chainID string, maxChains int64, fn func(store.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.Create(ctx, chainID, maxChains, fn)
}

func TestWorkOutlivesTheClient(t *testing.T) {
	h := Handler(baseConfig(cancelCheckingStore{memory.New()}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v2/command/", bytes.NewReader(getUpdates(t)))
	req.Header.Set("Authorization", "Bearer "+newToken(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rsp := response(t, rec); rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
		t.Fatalf("request whose client left: %v", rsp.GetErrorCode())
	}
}

func TestAccessLogAndMetrics(t *testing.T) {
	var logs bytes.Buffer
	c := baseConfig(memory.New())
	c.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	c.AccessLog = true
	c.Metrics = metrics.New()
	h := Handler(c)
	token := newToken(t)
	do(h, request{path: "/v2/command/?client_id=secret-query", body: getUpdates(t), token: token})

	// A commit whose content must not reach the log.
	commit, _ := proto.Marshal(&pb.ClientToServerMessage{
		Share:           proto.String(""),
		MessageContents: pb.ClientToServerMessage_COMMIT.Enum(),
		Commit: &pb.CommitMessage{Entries: []*pb.SyncEntity{{
			IdString: proto.String("c1"), NonUniqueName: proto.String("secret-title"),
		}}},
	})
	do(h, request{path: "/v2/command/", body: commit, token: token})

	out := logs.String()
	for _, secret := range []string{"secret-title", "secret-query", token} {
		if strings.Contains(out, secret) {
			t.Errorf("the access log has %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, `"message":"get_updates"`) || !strings.Contains(out, `"result":"SUCCESS"`) {
		t.Errorf("the access log lacks the request: %s", out)
	}

	rec := httptest.NewRecorder()
	c.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	want := `arcomium_sync_requests_total{message="get_updates",result="SUCCESS",route="command"} 1`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("metrics lack %s", want)
	}
}
