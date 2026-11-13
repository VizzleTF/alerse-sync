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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/account"
	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/credential"
	"github.com/VizzleTF/arcomium-sync/internal/events"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/server"
	"github.com/VizzleTF/arcomium-sync/internal/store/postgres"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
	"github.com/VizzleTF/arcomium-sync/internal/testpg"
)

const bookmarkType = 32904

var (
	// baseURL is the server's.
	baseURL string
	pool    *pgxpool.Pool
	st      *postgres.Store
	testLog = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	var stop func()
	var err error
	pool, stop, err = testpg.Start(context.Background())
	if err != nil {
		fmt.Println(err)
		return 1
	}
	defer stop()
	st = postgres.New(pool)
	// Listeners end before Postgres stops, or it waits for them.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, hub := newInstance(ctx)
	srv.Start()
	defer srv.Close()
	defer hub.Close()
	baseURL = srv.URL
	return m.Run()
}

// newInstance returns a server instance on the shared database, set up as
// the command sets it up.
func newInstance(ctx context.Context) (*httptest.Server, *events.Hub) {
	hub := events.New(pool, testLog, 8, 100)
	go hub.Listen(ctx)
	service := protocol.New(st, hub, protocol.DefaultLimits)
	accounts := account.New(account.Config{
		Store: st, Hash: credential.Hash{Time: 1, Memory: 1024, Threads: 1},
		Cleared: service.Cleared, Log: testLog,
	})
	srv := httptest.NewUnstartedServer(server.Handler(server.Config{
		Service:  service,
		Events:   hub,
		Accounts: accounts,
		ChainExists: func(ctx context.Context, chainID string) (bool, error) {
			_, ok, err := st.Chain(ctx, chainID)
			return ok, err
		},
		Health: st.Ping,
		Log:    testLog,
	}))
	srv.Config.RegisterOnShutdown(hub.Close)
	return srv, hub
}

// device is a client of a sync chain, speaking the protocol over HTTP.
type device struct {
	t        *testing.T
	guid     string
	birthday string
	token    []byte
	// url is the chain's: <server>/v3/chains/<chain>.
	url string
	// newToken issues the device's tokens.
	newToken func() string
}

func (d *device) bearer() string { return d.newToken() }

func (d *device) server() string { return d.url }

// testAccount is an account whose devices sync one chain over /v3. The
// server checks nothing of the sk-v1 derivation, so random values stand in
// for the envelope id and the authentication key.
type testAccount struct {
	t        *testing.T
	id       string
	envelope string
	auth     []byte
	chain    string
	first    ed25519.PrivateKey // the creating device, until device() hands it out
}

func randomHex(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

// newAccount creates an account with one device on the shared server.
func newAccount(t *testing.T) *testAccount {
	t.Helper()
	a := &testAccount{t: t, envelope: randomHex(t), auth: make([]byte, 32), chain: randomHex(t)}
	_, _ = rand.Read(a.auth)
	_, a.first, _ = ed25519.GenerateKey(nil)
	status, body := v3(t, "POST", "/account", "", map[string]any{
		"id": a.envelope, "scheme": account.SchemeSK, "salt": make([]byte, 32), "kdf": credential.DefaultKDF,
		"auth": a.auth, "wrapped": make([]byte, 60),
		"device": map[string]any{"public_key": publicKey(a.first), "name": "First"},
	})
	var created struct{ Account string }
	if status != 201 || json.Unmarshal(body, &created) != nil {
		t.Fatalf("create account: HTTP %d: %s", status, body)
	}
	a.id = created.Account
	return a
}

// device returns a device of the account on its chain, served by server
// (baseURL if empty). After the first, each one signs a new key in.
func (a *testAccount) device(guid, server string) *device {
	a.t.Helper()
	if server == "" {
		server = baseURL
	}
	key := a.first
	if key != nil {
		a.first = nil
	} else {
		_, key, _ = ed25519.GenerateKey(nil)
		status, body := v3(a.t, "POST", "/unlock", "", map[string]any{
			"id": a.envelope, "auth": a.auth,
			"device": map[string]any{"public_key": publicKey(key), "name": guid},
		})
		if status != 200 {
			a.t.Fatalf("unlock: HTTP %d: %s", status, body)
		}
	}
	return &device{
		t: a.t, guid: guid, url: server + "/v3/chains/" + a.chain,
		newToken: func() string { return auth.NewDeviceToken(key, a.id, time.Now()) },
	}
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
	req, _ := http.NewRequest(http.MethodPost, d.server()+"/command/?client=Chromium", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+d.bearer())
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.server()+"/events?cache_guid="+d.guid, nil)
	req.Header.Set("Authorization", "Bearer "+d.bearer())
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
	acc := newAccount(t)
	a := acc.device("a", "")
	b := acc.device("b", "")
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
	other := newAccount(t).device("x", "")
	if got := len(other.bookmarks()); got != 4 {
		t.Fatalf("other chain: %d entities, want 4", got)
	}
}

func TestConcurrentCommits(t *testing.T) {
	acc := newAccount(t)
	reader := acc.device("reader", "")
	reader.bookmarks()

	const writers, commits = 4, 25
	devices := make([]*device, writers)
	for w := range writers {
		devices[w] = acc.device(fmt.Sprintf("w%d", w), "")
	}
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			d := devices[w]
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
	acc := newAccount(t)
	a := acc.device("a", "")
	b := acc.device("b", "")
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
	fresh := acc.device("c", "")
	if got := len(fresh.bookmarks()); got != 4 {
		t.Fatalf("after clear: %d entities, want 4", got)
	}
}

func TestRejectsBadTokens(t *testing.T) {
	acc := newAccount(t)
	_, stranger, _ := ed25519.GenerateKey(nil)
	for name, header := range map[string]string{
		"none":           "",
		"garbage":        "Bearer abc",
		"unknown device": "Bearer " + auth.NewDeviceToken(stranger, acc.id, time.Now()),
		"expired":        "Bearer " + auth.NewDeviceToken(stranger, acc.id, time.Now().Add(-48*time.Hour)),
	} {
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/v3/chains/"+acc.chain+"/command/", nil)
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
	// The removed scheme answers nothing.
	resp, err := http.Post(baseURL+"/v2/command/", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/v2/command/: HTTP %d, want 404", resp.StatusCode)
	}
}
