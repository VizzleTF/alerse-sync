package pairing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/clientip"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	"github.com/VizzleTF/arcomium-sync/internal/store/memory"
)

type env struct {
	t     *testing.T
	h     http.Handler
	store *memory.Store
	now   time.Time
}

func newEnv(t *testing.T, c Config) *env {
	t.Helper()
	e := &env{t: t, store: memory.New(), now: time.Now()}
	e.store.Now = func() time.Time { return e.now }
	c.Store = e.store
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	// The client address, which the per-address limits count.
	e.h = (*clientip.Resolver)(nil).Middleware(Handler(c))
	return e
}

// chain returns a token of a new chain that exists on the server.
func (e *env) chain() string {
	e.t.Helper()
	public, key, _ := ed25519.GenerateKey(nil)
	if err := protocol.New(e.store, nil, protocol.DefaultLimits).EnsureChain(context.Background(), auth.ChainID(public)); err != nil {
		e.t.Fatal(err)
	}
	return auth.NewToken(key, time.Now())
}

func (e *env) do(method, path, token string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func slotID(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func fill(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func (e *env) slot(id string) (int, slotResponse) {
	e.t.Helper()
	rec := e.do("GET", "/v2/pair/"+id, "", nil)
	var rsp slotResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &rsp)
	return rec.Code, rsp
}

func TestPairing(t *testing.T) {
	e := newEnv(t, Config{})
	token := e.chain()
	id := slotID("ABCD")

	if rec := e.do("POST", "/v2/pair", token, openRequest{Slot: id, Msg: fill(1, 32)}); rec.Code != 204 {
		t.Fatalf("open: HTTP %d: %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v2/pair", e.chain(), openRequest{Slot: id, Msg: fill(1, 32)}); rec.Code != 409 {
		t.Fatalf("open a taken slot: HTTP %d", rec.Code)
	}
	code, rsp := e.slot(id)
	if code != 200 || !bytes.Equal(rsp.Msg, fill(1, 32)) || rsp.Joined || rsp.Expires < 290 {
		t.Fatalf("get: HTTP %d %+v", code, rsp)
	}

	// Approving before the new device joined does nothing.
	if rec := e.do("POST", "/v2/pair/"+id+"/approve", token, approveRequest{Box: fill(9, 61)}); rec.Code != 409 {
		t.Fatalf("approve before join: HTTP %d", rec.Code)
	}
	join := joinRequest{Msg: fill(2, 32), Name: "Laptop", Confirm: fill(3, 32)}
	if rec := e.do("POST", "/v2/pair/"+id+"/join", "", join); rec.Code != 204 {
		t.Fatalf("join: HTTP %d: %s", rec.Code, rec.Body)
	}
	// One join per slot: a second device, or an attacker, cannot answer.
	if rec := e.do("POST", "/v2/pair/"+id+"/join", "", join); rec.Code != 409 {
		t.Fatalf("second join: HTTP %d", rec.Code)
	}
	code, rsp = e.slot(id)
	if code != 200 || !rsp.Joined || rsp.Name != "Laptop" || !bytes.Equal(rsp.BMsg, fill(2, 32)) ||
		!bytes.Equal(rsp.Confirm, fill(3, 32)) || rsp.Box != nil {
		t.Fatalf("get after join: %+v", rsp)
	}

	// Only the chain of the slot approves.
	if rec := e.do("POST", "/v2/pair/"+id+"/approve", e.chain(), approveRequest{Box: fill(9, 61)}); rec.Code != 404 {
		t.Fatalf("approve by another chain: HTTP %d", rec.Code)
	}
	if rec := e.do("POST", "/v2/pair/"+id+"/approve", token, approveRequest{Box: fill(9, 61)}); rec.Code != 204 {
		t.Fatalf("approve: HTTP %d", rec.Code)
	}
	if rec := e.do("POST", "/v2/pair/"+id+"/approve", token, approveRequest{Box: fill(8, 61)}); rec.Code != 409 {
		t.Fatalf("second approve: HTTP %d", rec.Code)
	}
	if _, rsp = e.slot(id); !bytes.Equal(rsp.Box, fill(9, 61)) {
		t.Fatalf("box: %x", rsp.Box)
	}

	if rec := e.do("DELETE", "/v2/pair/"+id, token, nil); rec.Code != 204 {
		t.Fatalf("delete: HTTP %d", rec.Code)
	}
	if code, _ := e.slot(id); code != 404 {
		t.Fatalf("get after delete: HTTP %d", code)
	}
}

func TestPairingExpires(t *testing.T) {
	e := newEnv(t, Config{})
	token := e.chain()
	id := slotID("EFGH")
	e.do("POST", "/v2/pair", token, openRequest{Slot: id, Msg: fill(1, 32)})
	e.now = e.now.Add(TTL)
	if code, _ := e.slot(id); code != 404 {
		t.Fatalf("expired slot: HTTP %d", code)
	}
	if rec := e.do("POST", "/v2/pair/"+id+"/join", "", joinRequest{Msg: fill(2, 32), Name: "x", Confirm: fill(3, 32)}); rec.Code != 404 {
		t.Fatalf("join expired: HTTP %d", rec.Code)
	}
	// An expired slot ID can be opened again.
	if rec := e.do("POST", "/v2/pair", token, openRequest{Slot: id, Msg: fill(1, 32)}); rec.Code != 204 {
		t.Fatalf("reopen: HTTP %d", rec.Code)
	}
}

func TestPairingLimits(t *testing.T) {
	e := newEnv(t, Config{Opens: ratelimit.NewPer(1, time.Hour), Joins: ratelimit.NewPer(1, time.Hour)})
	token := e.chain()
	e.do("POST", "/v2/pair", token, openRequest{Slot: slotID("1"), Msg: fill(1, 32)})
	if rec := e.do("POST", "/v2/pair", token, openRequest{Slot: slotID("2"), Msg: fill(1, 32)}); rec.Code != 429 {
		t.Fatalf("second open: HTTP %d", rec.Code)
	}
	join := joinRequest{Msg: fill(2, 32), Name: "x", Confirm: fill(3, 32)}
	e.do("POST", "/v2/pair/"+slotID("9")+"/join", "", join)
	if rec := e.do("POST", "/v2/pair/"+slotID("1")+"/join", "", join); rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("join over the limit: HTTP %d", rec.Code)
	}
}

func TestPairingBadRequests(t *testing.T) {
	e := newEnv(t, Config{})
	token := e.chain()
	id := slotID("BAD")
	e.do("POST", "/v2/pair", token, openRequest{Slot: id, Msg: fill(1, 32)})
	for name, test := range map[string]struct {
		method, path, token string
		body                any
		status              int
	}{
		"open without token": {"POST", "/v2/pair", "", openRequest{Slot: slotID("x"), Msg: fill(1, 32)}, 401},
		"open short msg":     {"POST", "/v2/pair", token, openRequest{Slot: slotID("x"), Msg: fill(1, 3)}, 400},
		"open bad slot":      {"POST", "/v2/pair", token, openRequest{Slot: "abc", Msg: fill(1, 32)}, 400},
		"join no name":       {"POST", "/v2/pair/" + id + "/join", "", joinRequest{Msg: fill(2, 32), Confirm: fill(3, 32)}, 400},
		"join long name":     {"POST", "/v2/pair/" + id + "/join", "", joinRequest{Msg: fill(2, 32), Name: string(fill('a', 65)), Confirm: fill(3, 32)}, 400},
		"join short confirm": {"POST", "/v2/pair/" + id + "/join", "", joinRequest{Msg: fill(2, 32), Name: "x", Confirm: fill(3, 4)}, 400},
		"approve no token":   {"POST", "/v2/pair/" + id + "/approve", "", approveRequest{Box: fill(1, 10)}, 401},
		"approve huge box":   {"POST", "/v2/pair/" + id + "/approve", token, approveRequest{Box: fill(1, 300)}, 400},
		"delete no token":    {"DELETE", "/v2/pair/" + id, "", nil, 401},
		"get bad slot":       {"GET", "/v2/pair/zz", "", nil, 400},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := e.do(test.method, test.path, test.token, test.body); rec.Code != test.status {
				t.Fatalf("HTTP %d, want %d: %s", rec.Code, test.status, rec.Body)
			}
		})
	}
}
