package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/guard"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	"github.com/VizzleTF/arcomium-sync/internal/store/memory"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

// TestGuard checks that an address that keeps sending refused requests is
// shut out, also for requests that would pass, and others are not.
func TestGuard(t *testing.T) {
	st := memory.New()
	c := baseConfig(st)
	c.Guard = guard.New(3, time.Minute, time.Hour)
	h := Handler(c)
	cmd, _, token := testChain(t, st)
	from := func(addr string, r request) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, r.path, bytes.NewReader(r.body))
		req.RemoteAddr = addr + ":1234"
		if r.token != "" {
			req.Header.Set("Authorization", "Bearer "+r.token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	good := request{path: cmd + "/", body: getUpdates(t), token: token}
	for i, bad := range []request{
		{path: cmd + "/", body: getUpdates(t), token: "garbage"},
		{path: cmd + "/", body: []byte("not a protobuf"), token: token},
		{path: "/v3/nothing", token: token},
	} {
		if rec := from("192.0.2.1", bad); rec.Code < 400 || rec.Code == http.StatusTooManyRequests {
			t.Fatalf("bad request %d: HTTP %d", i, rec.Code)
		}
	}
	if rec := from("192.0.2.1", good); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("a good request of a shut out address: HTTP %d", rec.Code)
	}
	if rec := from("192.0.2.2", good); rec.Code != http.StatusOK {
		t.Fatalf("another address: HTTP %d", rec.Code)
	}
}

func TestCommitLimit(t *testing.T) {
	st := memory.New()
	c := baseConfig(st)
	c.Commits = ratelimit.NewBurst(1, time.Hour, 2)
	h := Handler(c)
	cmd, _, token := testChain(t, st)
	if rsp := response(t, do(h, request{path: cmd + "/", body: getUpdates(t), token: token})); rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
		t.Fatalf("GetUpdates: %v", rsp.GetErrorCode())
	}
	commit, err := proto.Marshal(&pb.ClientToServerMessage{
		Share:           proto.String(""),
		MessageContents: pb.ClientToServerMessage_COMMIT.Enum(),
		Commit:          &pb.CommitMessage{CacheGuid: proto.String("a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var codes []pb.SyncEnums_ErrorType
	for range 3 {
		codes = append(codes, response(t, do(h, request{path: cmd + "/", body: commit, token: token})).GetErrorCode())
	}
	if codes[2] != pb.SyncEnums_THROTTLED || codes[0] == pb.SyncEnums_THROTTLED || codes[1] == pb.SyncEnums_THROTTLED {
		t.Fatalf("commits answered %v; want the third THROTTLED", codes)
	}
	// GetUpdates are not commits.
	if rsp := response(t, do(h, request{path: cmd + "/", body: getUpdates(t), token: token})); rsp.GetErrorCode() == pb.SyncEnums_THROTTLED {
		t.Fatal("GetUpdates throttled by the commit limit")
	}
}

func TestDeclaredTooLarge(t *testing.T) {
	st := memory.New()
	c := baseConfig(st)
	c.MaxRequestBytes = 1024
	h := Handler(c)
	cmd, _, token := testChain(t, st)
	req := httptest.NewRequest(http.MethodPost, cmd+"/", bytes.NewReader(make([]byte, 2048)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a 2 KiB body over a 1 KiB limit: HTTP %d", rec.Code)
	}
}
