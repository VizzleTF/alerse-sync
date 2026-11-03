//go:build integration

// End-to-end test of the sync server over HTTP against a real Postgres, which
// the test starts itself:
//
//	go test -tags integration ./e2e/
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/events"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/server"
	"github.com/VizzleTF/arcomium-sync/internal/store/postgres"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

const bookmarkType = 32904

var serverURL string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	port := freePort()
	dir, err := os.MkdirTemp("", "arcomium-sync-e2e")
	if err != nil {
		fmt.Println(err)
		return 1
	}
	defer os.RemoveAll(dir)
	db := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)).
		RuntimePath(filepath.Join(dir, "runtime")).
		DataPath(filepath.Join(dir, "data")).
		Logger(io.Discard))
	if err := db.Start(); err != nil {
		fmt.Println("start postgres:", err)
		return 1
	}
	defer db.Stop() //nolint:errcheck // the test is over

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool, err := pgxpool.New(ctx, fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres", port))
	if err != nil {
		fmt.Println(err)
		return 1
	}
	defer pool.Close()
	st := postgres.New(pool)
	// Running the migrations twice must be harmless: every start runs them.
	for range 2 {
		if err := st.Migrate(ctx); err != nil {
			fmt.Println("migrate:", err)
			return 1
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	hub := events.New(pool, log, 8, 100)
	go hub.Listen(ctx)
	srv := httptest.NewUnstartedServer(server.Handler(server.Config{
		Service: protocol.New(st, hub, protocol.DefaultLimits),
		Events:  hub,
		Health:  st.Ping,
		Log:     log,
	}))
	srv.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	srv.Start()
	// Event streams end with ctx, before Close waits for them.
	defer srv.Close()
	defer cancel()
	serverURL = srv.URL + "/v2"
	return m.Run()
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// device is a client of a sync chain, speaking the protocol over HTTP.
type device struct {
	t        *testing.T
	key      ed25519.PrivateKey
	guid     string
	birthday string
	token    []byte
}

func newChain(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func (d *device) send(msg *pb.ClientToServerMessage) *pb.ClientToServerResponse {
	d.t.Helper()
	msg.Share = proto.String("")
	if d.birthday != "" {
		msg.StoreBirthday = proto.String(d.birthday)
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		d.t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, serverURL+"/command/?client=Chromium", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+auth.NewToken(d.key, time.Now()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		d.t.Fatalf("command: HTTP %d: %s", resp.StatusCode, data)
	}
	rsp := &pb.ClientToServerResponse{}
	if err := proto.Unmarshal(data, rsp); err != nil {
		d.t.Fatal(err)
	}
	if rsp.GetStoreBirthday() != "" {
		d.birthday = rsp.GetStoreBirthday()
	}
	return rsp
}

// bookmarks fetches all bookmark changes since the device's last fetch.
func (d *device) bookmarks() []*pb.SyncEntity {
	d.t.Helper()
	var entities []*pb.SyncEntity
	for {
		rsp := d.send(&pb.ClientToServerMessage{
			MessageContents: pb.ClientToServerMessage_GET_UPDATES.Enum(),
			GetUpdates: &pb.GetUpdatesMessage{
				GetUpdatesOrigin: pb.SyncEnums_PERIODIC.Enum(),
				FromProgressMarker: []*pb.DataTypeProgressMarker{
					{DataTypeId: proto.Int32(bookmarkType), Token: d.token},
				},
			},
		})
		if rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
			d.t.Fatalf("GetUpdates: %v", rsp.GetErrorCode())
		}
		updates := rsp.GetGetUpdates()
		d.token = updates.GetNewProgressMarker()[0].GetToken()
		entities = append(entities, updates.GetEntries()...)
		if updates.GetChangesRemaining() == 0 {
			return entities
		}
	}
}

func (d *device) commit(entries ...*pb.SyncEntity) []*pb.CommitResponse_EntryResponse {
	d.t.Helper()
	rsp := d.send(&pb.ClientToServerMessage{
		MessageContents: pb.ClientToServerMessage_COMMIT.Enum(),
		Commit:          &pb.CommitMessage{Entries: entries, CacheGuid: proto.String(d.guid)},
	})
	if rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
		d.t.Fatalf("Commit: %v", rsp.GetErrorCode())
	}
	return rsp.GetCommit().GetEntryresponse()
}

func bookmark(id, title string) *pb.SyncEntity {
	return &pb.SyncEntity{
		IdString:       proto.String(id),
		ParentIdString: proto.String("bookmark_bar"),
		NonUniqueName:  proto.String(title),
		Specifics: &pb.EntitySpecifics{SpecificsVariant: &pb.EntitySpecifics_Bookmark{
			Bookmark: &pb.BookmarkSpecifics{LegacyCanonicalizedTitle: proto.String(title)},
		}},
	}
}

// events opens the device's event stream and returns its "event data" lines.
func (d *device) events(ctx context.Context) <-chan string {
	d.t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/events?cache_guid="+d.guid, nil)
	req.Header.Set("Authorization", "Bearer "+auth.NewToken(d.key, time.Now()))
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the reader below closes it
	if err != nil {
		d.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		d.t.Fatalf("events: HTTP %d", resp.StatusCode)
	}
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		var event string
		for scanner.Scan() {
			if name, ok := strings.CutPrefix(scanner.Text(), "event: "); ok {
				event = name
			} else if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				lines <- event + " " + data
			}
		}
	}()
	return lines
}

func expectEvent(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case got := <-lines:
		if got != want {
			t.Fatalf("event %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no event %q", want)
	}
}

func TestTwoDevices(t *testing.T) {
	key := newChain(t)
	a := &device{t: t, key: key, guid: "a"}
	b := &device{t: t, key: key, guid: "b"}
	if got := len(a.bookmarks()); got != 4 {
		t.Fatalf("first sync: %d bookmark entities, want the 4 permanent ones", got)
	}
	b.bookmarks()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := b.events(ctx)
	expectEvent(t, stream, "ready {}")

	created := a.commit(bookmark("c1", "Example"))[0]
	if created.GetResponseType() != pb.CommitResponse_SUCCESS {
		t.Fatalf("commit: %v", created.GetResponseType())
	}
	expectEvent(t, stream, `changed {"types":[32904]}`)
	got := b.bookmarks()
	if len(got) != 1 || got[0].GetNonUniqueName() != "Example" || got[0].GetIdString() != created.GetIdString() {
		t.Fatalf("b fetched %v", got)
	}

	// Both edit version 1; the second one conflicts.
	edit := bookmark(created.GetIdString(), "Edited")
	edit.Version = proto.Int64(created.GetVersion())
	if r := b.commit(edit)[0]; r.GetResponseType() != pb.CommitResponse_SUCCESS {
		t.Fatalf("b's edit: %v", r.GetResponseType())
	}
	if r := a.commit(edit)[0]; r.GetResponseType() != pb.CommitResponse_CONFLICT {
		t.Fatalf("a's stale edit: %v, want CONFLICT", r.GetResponseType())
	}

	// Another chain sees none of it.
	other := &device{t: t, key: newChain(t), guid: "x"}
	if got := len(other.bookmarks()); got != 4 {
		t.Fatalf("other chain: %d entities, want 4", got)
	}
}

func TestConcurrentCommits(t *testing.T) {
	key := newChain(t)
	reader := &device{t: t, key: key, guid: "reader"}
	reader.bookmarks()

	const writers, commits = 4, 25
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			d := &device{t: t, key: key, guid: fmt.Sprintf("w%d", w)}
			d.bookmarks()
			for c := range commits {
				d.commit(bookmark(fmt.Sprintf("c%d", c), fmt.Sprintf("%d-%d", w, c)))
			}
		})
	}
	wg.Wait()

	got := reader.bookmarks()
	if len(got) != writers*commits {
		t.Fatalf("reader got %d bookmarks, want %d", len(got), writers*commits)
	}
	for i := 1; i < len(got); i++ {
		if got[i].GetVersion() <= got[i-1].GetVersion() {
			t.Fatalf("versions out of order: %d after %d", got[i].GetVersion(), got[i-1].GetVersion())
		}
	}
}

func TestClearServerData(t *testing.T) {
	key := newChain(t)
	a := &device{t: t, key: key, guid: "a"}
	b := &device{t: t, key: key, guid: "b"}
	a.bookmarks()
	b.bookmarks()
	a.commit(bookmark("c1", "Example"))

	rsp := a.send(&pb.ClientToServerMessage{
		MessageContents: pb.ClientToServerMessage_CLEAR_SERVER_DATA.Enum(),
		ClearServerData: &pb.ClearServerDataMessage{},
	})
	if rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
		t.Fatalf("clear: %v", rsp.GetErrorCode())
	}
	stale := b.send(&pb.ClientToServerMessage{
		MessageContents: pb.ClientToServerMessage_COMMIT.Enum(),
		Commit:          &pb.CommitMessage{Entries: []*pb.SyncEntity{bookmark("c2", "x")}},
	})
	if stale.GetErrorCode() != pb.SyncEnums_NOT_MY_BIRTHDAY {
		t.Fatalf("commit after clear: %v, want NOT_MY_BIRTHDAY", stale.GetErrorCode())
	}
	fresh := &device{t: t, key: key, guid: "c"}
	if got := len(fresh.bookmarks()); got != 4 {
		t.Fatalf("after clear: %d entities, want 4", got)
	}
}

func TestRejectsBadTokens(t *testing.T) {
	_, other, _ := ed25519.GenerateKey(nil)
	for name, header := range map[string]string{
		"none":    "",
		"garbage": "Bearer abc",
		"expired": "Bearer " + auth.NewToken(other, time.Now().Add(-48*time.Hour)),
	} {
		req, _ := http.NewRequest(http.MethodPost, serverURL+"/command/", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s token: HTTP %d, want 401", name, resp.StatusCode)
		}
	}
}
