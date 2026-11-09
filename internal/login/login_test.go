package login

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

// cheap keeps the tests fast; the cost does not change what is tested.
var cheap = Hash{Time: 1, Memory: 64, Threads: 1}

type env struct {
	t     *testing.T
	h     http.Handler
	store *memory.Store
}

func newEnv(t *testing.T, c Config) *env {
	t.Helper()
	st := memory.New()
	service := protocol.New(st, nil, protocol.DefaultLimits)
	c.Store = st
	c.EnsureChain = service.EnsureChain
	c.Hash = cheap
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return &env{t: t, h: (*clientip.Resolver)(nil).Middleware(Handler(c)), store: st}
}

func (e *env) do(method, path, token string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			e.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func loginID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

func newToken(t *testing.T) (string, string) {
	t.Helper()
	public, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewToken(key, time.Now()), auth.ChainID(public)
}

func record(id string, authKey byte) setRequest {
	return setRequest{
		ID: id, Scheme: Scheme, KDF: DefaultKDF,
		Salt:   bytes.Repeat([]byte{7}, 32),
		Auth:   bytes.Repeat([]byte{authKey}, 32),
		Secret: bytes.Repeat([]byte{9}, 60),
	}
}

func TestLifecycle(t *testing.T) {
	e := newEnv(t, Config{})
	token, chainID := newToken(t)
	id := loginID("alice@example.com")

	if rec := e.do("GET", "/v2/login", token, nil); rec.Code != 404 {
		t.Fatalf("get before set: HTTP %d", rec.Code)
	}
	// The chain does not exist yet: setting a login creates it.
	if rec := e.do("PUT", "/v2/login", token, record(id, 1)); rec.Code != 204 {
		t.Fatalf("set: HTTP %d: %s", rec.Code, rec.Body)
	}
	if _, ok, _ := e.store.Chain(context.Background(), chainID); !ok {
		t.Fatal("set did not create the chain")
	}

	rec := e.do("GET", "/v2/login", token, nil)
	var got getResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.ID != id || got.KDF != DefaultKDF {
		t.Fatalf("get: HTTP %d: %s", rec.Code, rec.Body)
	}

	rec = e.do("POST", "/v2/login/params", "", paramsRequest{ID: id})
	var params paramsResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &params) != nil ||
		!bytes.Equal(params.Salt, bytes.Repeat([]byte{7}, 32)) || params.Scheme != Scheme {
		t.Fatalf("params: HTTP %d: %s", rec.Code, rec.Body)
	}

	rec = e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: id, Auth: bytes.Repeat([]byte{1}, 32)})
	var unlocked unlockResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &unlocked) != nil ||
		!bytes.Equal(unlocked.Secret, bytes.Repeat([]byte{9}, 60)) {
		t.Fatalf("unlock: HTTP %d: %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: id, Auth: bytes.Repeat([]byte{2}, 32)}); rec.Code != 401 {
		t.Fatalf("unlock with a wrong key: HTTP %d", rec.Code)
	}

	// A new password replaces the old one.
	if rec := e.do("PUT", "/v2/login", token, record(id, 3)); rec.Code != 204 {
		t.Fatalf("change: HTTP %d", rec.Code)
	}
	if rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: id, Auth: bytes.Repeat([]byte{1}, 32)}); rec.Code != 401 {
		t.Fatalf("old password after change: HTTP %d", rec.Code)
	}
	if rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: id, Auth: bytes.Repeat([]byte{3}, 32)}); rec.Code != 200 {
		t.Fatalf("new password: HTTP %d", rec.Code)
	}

	// A new login name replaces the chain's old one.
	other := loginID("bob")
	if rec := e.do("PUT", "/v2/login", token, record(other, 3)); rec.Code != 204 {
		t.Fatalf("rename: HTTP %d", rec.Code)
	}
	if _, ok, _ := e.store.Login(context.Background(), id); ok {
		t.Fatal("the old login name stayed")
	}

	if rec := e.do("DELETE", "/v2/login", token, nil); rec.Code != 204 {
		t.Fatalf("delete: HTTP %d", rec.Code)
	}
	if rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: other, Auth: bytes.Repeat([]byte{3}, 32)}); rec.Code != 401 {
		t.Fatalf("unlock after delete: HTTP %d", rec.Code)
	}
}

func TestStoredVerifierIsSlowHash(t *testing.T) {
	e := newEnv(t, Config{})
	token, _ := newToken(t)
	id := loginID("carol")
	req := record(id, 5)
	e.do("PUT", "/v2/login", token, req)
	l, ok, _ := e.store.Login(context.Background(), id)
	if !ok {
		t.Fatal("no login")
	}
	fast := sha256.Sum256(req.Auth)
	if bytes.Equal(l.Verifier, req.Auth) || bytes.Equal(l.Verifier, fast[:]) || len(l.VerifierSalt) != 16 {
		t.Fatal("the verifier is the key or a fast hash of it")
	}
}

func TestTakenLogin(t *testing.T) {
	e := newEnv(t, Config{})
	first, _ := newToken(t)
	second, _ := newToken(t)
	id := loginID("dave")
	if rec := e.do("PUT", "/v2/login", first, record(id, 1)); rec.Code != 204 {
		t.Fatalf("first: HTTP %d", rec.Code)
	}
	if rec := e.do("PUT", "/v2/login", second, record(id, 2)); rec.Code != 409 {
		t.Fatalf("second chain, same login: HTTP %d", rec.Code)
	}
	if rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: id, Auth: bytes.Repeat([]byte{1}, 32)}); rec.Code != 200 {
		t.Fatalf("the first chain's login changed: HTTP %d", rec.Code)
	}
}

func TestNoEnumeration(t *testing.T) {
	e := newEnv(t, Config{})
	unknown := loginID("nobody")
	first := e.do("POST", "/v2/login/params", "", paramsRequest{ID: unknown})
	second := e.do("POST", "/v2/login/params", "", paramsRequest{ID: unknown})
	var a, b paramsResponse
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &a) != nil || json.Unmarshal(second.Body.Bytes(), &b) != nil {
		t.Fatalf("params of an unknown login: HTTP %d", first.Code)
	}
	if len(a.Salt) != 32 || !bytes.Equal(a.Salt, b.Salt) || a.KDF != DefaultKDF || a.Scheme != Scheme {
		t.Fatalf("made-up params differ or look fake: %+v %+v", a, b)
	}
	other := e.do("POST", "/v2/login/params", "", paramsRequest{ID: loginID("someone else")})
	var c paramsResponse
	_ = json.Unmarshal(other.Body.Bytes(), &c)
	if bytes.Equal(a.Salt, c.Salt) {
		t.Fatal("two login names got the same made-up salt")
	}
	rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: unknown, Auth: make([]byte, 32)})
	if rec.Code != 401 || !bytes.Contains(rec.Body.Bytes(), []byte("wrong login or password")) {
		t.Fatalf("unlock of an unknown login: HTTP %d: %s", rec.Code, rec.Body)
	}
}

func TestAttemptsLimited(t *testing.T) {
	e := newEnv(t, Config{Attempts: ratelimit.NewPer(3, time.Hour)})
	token, _ := newToken(t)
	id := loginID("erin")
	e.do("PUT", "/v2/login", token, record(id, 1))
	for i := range 3 {
		if rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: id, Auth: make([]byte, 32)}); rec.Code != 401 {
			t.Fatalf("attempt %d: HTTP %d", i, rec.Code)
		}
	}
	// Even the right password waits now.
	rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: id, Auth: bytes.Repeat([]byte{1}, 32)})
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("attempt over the limit: HTTP %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// Another login name is not affected...
	if rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: loginID("frank"), Auth: make([]byte, 32)}); rec.Code != 401 {
		t.Fatalf("other login: HTTP %d", rec.Code)
	}
}

func TestAttemptsLimitedPerAddress(t *testing.T) {
	e := newEnv(t, Config{AddrAttempts: ratelimit.NewPer(2, time.Hour)})
	for i, name := range []string{"a", "b"} {
		if rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: loginID(name), Auth: make([]byte, 32)}); rec.Code != 401 {
			t.Fatalf("attempt %d: HTTP %d", i, rec.Code)
		}
	}
	// A third login name from the same address waits.
	if rec := e.do("POST", "/v2/login/unlock", "", unlockRequest{ID: loginID("c"), Auth: make([]byte, 32)}); rec.Code != 429 {
		t.Fatalf("over the address limit: HTTP %d", rec.Code)
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t, Config{})
	token, _ := newToken(t)
	id := loginID("gina")
	weak := record(id, 1)
	weak.KDF.N = 1 << 10
	huge := record(id, 1)
	huge.KDF.N = 1 << 24
	scheme := record(id, 1)
	scheme.Scheme = "opaque"
	shortSalt := record(id, 1)
	shortSalt.Salt = []byte{1}
	shortAuth := record(id, 1)
	shortAuth.Auth = []byte{1}
	upper := record("ABCDEF"+id[6:], 1)

	for name, test := range map[string]struct {
		method, path, token string
		body                any
		status              int
	}{
		"weak kdf":          {"PUT", "/v2/login", token, weak, 400},
		"huge kdf":          {"PUT", "/v2/login", token, huge, 400},
		"unknown scheme":    {"PUT", "/v2/login", token, scheme, 400},
		"short salt":        {"PUT", "/v2/login", token, shortSalt, 400},
		"short auth":        {"PUT", "/v2/login", token, shortAuth, 400},
		"uppercase id":      {"PUT", "/v2/login", token, upper, 400},
		"no token":          {"PUT", "/v2/login", "", record(id, 1), 401},
		"delete no token":   {"DELETE", "/v2/login", "", nil, 401},
		"bad id":            {"POST", "/v2/login/params", "", paramsRequest{ID: "alice"}, 400},
		"not json":          {"POST", "/v2/login/unlock", "", "x", 400},
		"wrong method":      {"GET", "/v2/login/unlock", "", nil, 405},
		"unlock short auth": {"POST", "/v2/login/unlock", "", unlockRequest{ID: id, Auth: []byte{1}}, 400},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := e.do(test.method, test.path, test.token, test.body); rec.Code != test.status {
				t.Fatalf("HTTP %d, want %d: %s", rec.Code, test.status, rec.Body)
			}
		})
	}
}
