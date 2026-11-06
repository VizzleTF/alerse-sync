//go:build integration

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

// Devices on different server instances hear of each other's commits
// through Postgres NOTIFY.
func TestTwoInstances(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	other, hub := newInstance(ctx)
	other.Start()
	defer other.Close()
	defer hub.Close()

	key := newChain(t)
	a := &device{t: t, key: key, guid: "a"}
	b := &device{t: t, key: key, guid: "b", url: other.URL + "/v2"}
	a.bookmarks()
	b.bookmarks()
	stream := b.events(ctx)
	expectEvent(t, stream, "ready {}")
	a.commit(bookmark("c1", "Example"))
	expectEvent(t, stream, `changed {"types":[32904]}`)
	if got := b.bookmarks(); len(got) != 1 {
		t.Fatalf("b fetched %d bookmarks from its instance", len(got))
	}
}

// Shutdown ends event streams right away but lets a commit in progress
// finish.
func TestShutdownWaitsForCommits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, hub := newInstance(ctx)
	srv.Start()
	defer hub.Close()

	key := newChain(t)
	a := &device{t: t, key: key, guid: "a", url: srv.URL + "/v2"}
	a.bookmarks()
	stream := a.events(ctx)
	expectEvent(t, stream, "ready {}")

	// A commit whose body arrives in two parts, the second after shutdown
	// began.
	msg := &pb.ClientToServerMessage{
		Share:           proto.String(""),
		StoreBirthday:   proto.String(a.birthday),
		MessageContents: pb.ClientToServerMessage_COMMIT.Enum(),
		Commit:          &pb.CommitMessage{Entries: []*pb.SyncEntity{bookmark("c1", "Kept")}, CacheGuid: proto.String("a")},
	}
	body, _ := proto.Marshal(msg)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /v2/command/ HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n",
		auth.NewToken(key, time.Now()), len(body))
	conn.Write(body[:len(body)/2]) //nolint:errcheck // checked by the response
	time.Sleep(100 * time.Millisecond)

	shutdown := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdown <- srv.Config.Shutdown(ctx)
	}()
	select {
	case _, open := <-stream:
		if open {
			t.Fatal("got an event during shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event stream stays open during shutdown")
	}
	conn.Write(body[len(body)/2:]) //nolint:errcheck // checked by the response
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	rsp := &pb.ClientToServerResponse{}
	if err := proto.Unmarshal(data, rsp); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("commit during shutdown: HTTP %d, %v", resp.StatusCode, err)
	}
	if got := rsp.GetCommit().GetEntryresponse(); len(got) != 1 || got[0].GetResponseType() != pb.CommitResponse_SUCCESS {
		t.Fatalf("commit during shutdown: %v", got)
	}
	if err := <-shutdown; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// The commit is there for the next instance.
	if got := (&device{t: t, key: key, guid: "b"}).bookmarks(); len(got) != 5 {
		t.Fatalf("after shutdown: %d bookmark entities, want 5", len(got))
	}
}
