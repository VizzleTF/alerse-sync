package account

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/clientip"
	"github.com/VizzleTF/arcomium-sync/internal/credential"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	"github.com/VizzleTF/arcomium-sync/internal/store"
	"github.com/VizzleTF/arcomium-sync/internal/store/memory"
)

// cheap keeps the tests fast; the cost does not change what is tested.
var cheap = credential.Hash{Time: 1, Memory: 64, Threads: 1}

// testStore keeps accounts and chains.
type testStore interface {
	store.Store
	store.AccountStore
}

// newStore returns an empty store for a test: in memory, or in Postgres
// with -tags integration.
var newStore = func(*testing.T) testStore { return memory.New() }

type env struct {
	t        *testing.T
	h        http.Handler
	store    testStore
	accounts *Accounts
	cleared  []string
}

func newEnv(t *testing.T, c Config) *env {
	t.Helper()
	e := &env{t: t, store: newStore(t)}
	c.Store = e.store
	c.Hash = cheap
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	c.Cleared = func(_ context.Context, chainID string) { e.cleared = append(e.cleared, chainID) }
	e.accounts = New(c)
	mux := http.NewServeMux()
	mux.Handle("/v3/", e.accounts.Handler())
	// A stand-in for the protocol: it answers the chain the request reached.
	mux.Handle("GET /v3/chains/{chain}/probe", e.accounts.Authenticate(e.accounts.ChainAccess(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, auth.ChainFromContext(r.Context()))
		}))))
	e.h = (*clientip.Resolver)(nil).Middleware(mux)
	return e
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

func id(b byte) string { return strings.Repeat(hex.EncodeToString([]byte{b}), 32) }

func newDevice(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func info(key ed25519.PrivateKey, name string) deviceInfo {
	return deviceInfo{PublicKey: key.Public().(ed25519.PublicKey), Name: name}
}

func createBody(envelope string, authKey byte, device ed25519.PrivateKey) createRequest {
	return createRequest{
		ID: envelope, Scheme: SchemeSK, KDF: credential.DefaultKDF,
		Salt:    bytes.Repeat([]byte{7}, 32),
		Auth:    bytes.Repeat([]byte{authKey}, 32),
		Wrapped: bytes.Repeat([]byte{9}, 61),
		Device:  info(device, "First laptop"),
	}
}

// create makes an account with auth key 3 and returns its ID.
func (e *env) create(envelope string, device ed25519.PrivateKey) string {
	e.t.Helper()
	rec := e.do("POST", "/v3/account", "", createBody(envelope, 3, device))
	var rsp createResponse
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &rsp) != nil {
		e.t.Fatalf("create: HTTP %d: %s", rec.Code, rec.Body)
	}
	return rsp.Account
}

func (e *env) unlock(envelope string, authKey byte, device ed25519.PrivateKey, name string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do("POST", "/v3/unlock", "", unlockRequest{
		ID: envelope, Auth: bytes.Repeat([]byte{authKey}, 32), Device: info(device, name),
	})
}

func token(key ed25519.PrivateKey, account string) string {
	return auth.NewDeviceToken(key, account, time.Now())
}

func TestCreateAndUnlock(t *testing.T) {
	e := newEnv(t, Config{})
	first, second := newDevice(t), newDevice(t)
	account := e.create(id(1), first)

	rec := e.do("POST", "/v3/unlock/params", "", paramsRequest{ID: id(1)})
	var params paramsResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &params) != nil {
		t.Fatalf("params: HTTP %d: %s", rec.Code, rec.Body)
	}
	if params.Scheme != SchemeSK || params.KDF != credential.DefaultKDF || !bytes.Equal(params.Salt, bytes.Repeat([]byte{7}, 32)) {
		t.Fatalf("params: %+v", params)
	}

	if rec := e.unlock(id(1), 4, second, "Phone"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: HTTP %d: %s", rec.Code, rec.Body)
	}
	// A device that failed to unlock did not join.
	if rec := e.do("GET", "/v3/account", token(second, account), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("device that failed: HTTP %d", rec.Code)
	}

	rec = e.unlock(id(1), 3, second, "Phone")
	var unlocked unlockResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &unlocked) != nil {
		t.Fatalf("unlock: HTTP %d: %s", rec.Code, rec.Body)
	}
	if unlocked.Account != account || !bytes.Equal(unlocked.Wrapped, bytes.Repeat([]byte{9}, 61)) {
		t.Fatalf("unlock: %+v", unlocked)
	}

	rec = e.do("GET", "/v3/account", token(second, account), nil)
	var got accountResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Account != account || got.Kind != "sk" {
		t.Fatalf("account: HTTP %d: %s", rec.Code, rec.Body)
	}

	// The words name one account.
	if rec := e.do("POST", "/v3/account", "", createBody(id(1), 5, newDevice(t))); rec.Code != http.StatusConflict {
		t.Fatalf("second account with the same words: HTTP %d", rec.Code)
	}
}

func TestCreateValidates(t *testing.T) {
	e := newEnv(t, Config{})
	weak := credential.DefaultKDF
	weak.N = 1 << 14
	tests := []struct {
		name   string
		change func(*createRequest)
	}{
		{"unknown scheme", func(r *createRequest) { r.Scheme = "pw-v1" }},
		{"kdf below the minimum", func(r *createRequest) { r.KDF = weak }},
		{"short salt", func(r *createRequest) { r.Salt = r.Salt[:8] }},
		{"short auth", func(r *createRequest) { r.Auth = r.Auth[:16] }},
		{"no wrapped key", func(r *createRequest) { r.Wrapped = nil }},
		{"short device key", func(r *createRequest) { r.Device.PublicKey = r.Device.PublicKey[:31] }},
		{"no device name", func(r *createRequest) { r.Device.Name = "" }},
		{"long device name", func(r *createRequest) { r.Device.Name = strings.Repeat("x", 65) }},
		{"control in device name", func(r *createRequest) { r.Device.Name = "a\nb" }},
		{"uppercase id", func(r *createRequest) { r.ID = strings.ToUpper(id(0xab)) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := createBody(id(2), 3, newDevice(t))
			test.change(&body)
			if rec := e.do("POST", "/v3/account", "", body); rec.Code != http.StatusBadRequest {
				t.Fatalf("HTTP %d: %s", rec.Code, rec.Body)
			}
		})
	}
}

// An id without an account looks like one with an account.
func TestNoEnumeration(t *testing.T) {
	e := newEnv(t, Config{})
	params := func(envelope string) paramsResponse {
		t.Helper()
		rec := e.do("POST", "/v3/unlock/params", "", paramsRequest{ID: envelope})
		var rsp paramsResponse
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &rsp) != nil {
			t.Fatalf("params: HTTP %d: %s", rec.Code, rec.Body)
		}
		return rsp
	}
	a, again, other := params(id(5)), params(id(5)), params(id(6))
	if a.Scheme != SchemeSK || a.KDF != credential.DefaultKDF || len(a.Salt) != 32 {
		t.Fatalf("made-up params: %+v", a)
	}
	if !bytes.Equal(a.Salt, again.Salt) || bytes.Equal(a.Salt, other.Salt) {
		t.Fatal("made-up salts must be stable per id and differ between ids")
	}
	if rec := e.unlock(id(5), 1, newDevice(t), "x"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unlock of no account: HTTP %d", rec.Code)
	}
}

func TestRateLimits(t *testing.T) {
	e := newEnv(t, Config{
		Creates:  ratelimit.NewPer(1, time.Hour),
		Lookups:  ratelimit.NewPer(1, time.Hour),
		Attempts: ratelimit.NewPer(2, time.Hour),
	})
	device := newDevice(t)
	account := e.create(id(1), device)
	if rec := e.do("POST", "/v3/account", "", createBody(id(2), 3, newDevice(t))); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second create: HTTP %d", rec.Code)
	}
	e.do("POST", "/v3/unlock/params", "", paramsRequest{ID: id(1)})
	if rec := e.do("POST", "/v3/unlock/params", "", paramsRequest{ID: id(1)}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second lookup: HTTP %d", rec.Code)
	}
	e.unlock(id(1), 4, newDevice(t), "x")
	// A device's password check counts as an attempt on the envelope.
	rec := e.do("DELETE", "/v3/account", token(device, account), authRequest{Auth: bytes.Repeat([]byte{4}, 32)})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password on delete: HTTP %d", rec.Code)
	}
	// Even the right password waits now.
	if rec := e.unlock(id(1), 3, newDevice(t), "x"); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("third attempt: HTTP %d", rec.Code)
	}
}

func TestDevices(t *testing.T) {
	e := newEnv(t, Config{MaxDevices: 3})
	laptop, phone, tablet := newDevice(t), newDevice(t), newDevice(t)
	account := e.create(id(1), laptop)
	for _, d := range []struct {
		key  ed25519.PrivateKey
		name string
	}{{phone, "Phone"}, {tablet, "Tablet"}} {
		if rec := e.unlock(id(1), 3, d.key, d.name); rec.Code != 200 {
			t.Fatalf("unlock %s: HTTP %d", d.name, rec.Code)
		}
	}
	if rec := e.unlock(id(1), 3, newDevice(t), "Fourth"); rec.Code != http.StatusConflict {
		t.Fatalf("over MaxDevices: HTTP %d", rec.Code)
	}
	// Unlocking again with the same key renames the device.
	if rec := e.unlock(id(1), 3, tablet, "Tablet 2"); rec.Code != 200 {
		t.Fatalf("unlock again: HTTP %d", rec.Code)
	}

	rec := e.do("GET", "/v3/devices", token(phone, account), nil)
	var list devicesResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("devices: HTTP %d: %s", rec.Code, rec.Body)
	}
	var names []string
	for _, d := range list.Devices {
		names = append(names, fmt.Sprintf("%s:%v", d.Name, d.Current))
	}
	if got := strings.Join(names, ","); got != "First laptop:false,Phone:true,Tablet 2:false" {
		t.Fatalf("devices: %s", got)
	}

	tabletID := hex.EncodeToString(tablet.Public().(ed25519.PublicKey))
	right, wrong := authRequest{Auth: bytes.Repeat([]byte{3}, 32)}, authRequest{Auth: bytes.Repeat([]byte{4}, 32)}
	if rec := e.do("DELETE", "/v3/devices/"+tabletID, token(phone, account), wrong); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remove with a wrong password: HTTP %d", rec.Code)
	}
	if rec := e.do("DELETE", "/v3/devices/"+tabletID, token(phone, account), right); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: HTTP %d: %s", rec.Code, rec.Body)
	}
	// The removed device is refused from its next request on, and its key
	// cannot join again.
	if rec := e.do("GET", "/v3/devices", token(tablet, account), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("removed device: HTTP %d", rec.Code)
	}
	if rec := e.unlock(id(1), 3, tablet, "Tablet"); rec.Code != http.StatusConflict {
		t.Fatalf("removed key unlocks: HTTP %d", rec.Code)
	}
	if rec := e.do("DELETE", "/v3/devices/"+tabletID, token(phone, account), right); rec.Code != http.StatusNotFound {
		t.Fatalf("remove twice: HTTP %d", rec.Code)
	}

	// A device may sign itself out, but the last one stays.
	phoneID := hex.EncodeToString(phone.Public().(ed25519.PublicKey))
	laptopID := hex.EncodeToString(laptop.Public().(ed25519.PublicKey))
	if rec := e.do("DELETE", "/v3/devices/"+phoneID, token(phone, account), right); rec.Code != http.StatusNoContent {
		t.Fatalf("remove itself: HTTP %d", rec.Code)
	}
	if rec := e.do("DELETE", "/v3/devices/"+laptopID, token(laptop, account), right); rec.Code != http.StatusConflict {
		t.Fatalf("remove the last device: HTTP %d", rec.Code)
	}

	// Removed records go after RemovedDeviceTTL, and then the key may join
	// again.
	done, err := e.store.Collect(t.Context(), store.Retention{
		Now: time.Now().Add(91 * 24 * time.Hour), RemovedDeviceTTL: 90 * 24 * time.Hour, Batch: 100,
	})
	if err != nil || done.RemovedDevices != 2 {
		t.Fatalf("collected %d device records, want 2: %v", done.RemovedDevices, err)
	}
	if rec := e.unlock(id(1), 3, tablet, "Tablet"); rec.Code != 200 {
		t.Fatalf("a key whose record went: HTTP %d", rec.Code)
	}
}

func TestDeviceTokens(t *testing.T) {
	e := newEnv(t, Config{})
	device := newDevice(t)
	account := e.create(id(1), device)
	for name, tok := range map[string]string{
		"none":          "",
		"garbage":       "abc",
		"other account": token(device, id(9)),
		"unknown key":   token(newDevice(t), account),
		"expired":       auth.NewDeviceToken(device, account, time.Now().Add(-25*time.Hour)),
	} {
		if rec := e.do("GET", "/v3/devices", tok, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: HTTP %d", name, rec.Code)
		}
	}
}

func TestChains(t *testing.T) {
	e := newEnv(t, Config{MaxChains: 2})
	alice, bob := newDevice(t), newDevice(t)
	aliceAccount := e.create(id(1), alice)
	bobAccount := e.create(id(2), bob)

	probe := func(key ed25519.PrivateKey, account, chainID string) *httptest.ResponseRecorder {
		t.Helper()
		return e.do("GET", "/v3/chains/"+chainID+"/probe", token(key, account), nil)
	}
	// A new chain becomes the account's on first use.
	if rec := probe(alice, aliceAccount, id(0xa1)); rec.Code != 200 || rec.Body.String() != id(0xa1) {
		t.Fatalf("first use: HTTP %d: %s", rec.Code, rec.Body)
	}
	if rec := probe(alice, aliceAccount, id(0xa1)); rec.Code != 200 {
		t.Fatalf("second use: HTTP %d", rec.Code)
	}
	if rec := probe(bob, bobAccount, id(0xa1)); rec.Code != http.StatusForbidden {
		t.Fatalf("another account's chain: HTTP %d", rec.Code)
	}
	if rec := probe(alice, aliceAccount, "A1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad chain ID: HTTP %d", rec.Code)
	}
	probe(alice, aliceAccount, id(0xa2))
	if rec := probe(alice, aliceAccount, id(0xa3)); rec.Code != http.StatusForbidden {
		t.Fatalf("over MaxChains: HTTP %d", rec.Code)
	}

	// A chain of no account (left by the old scheme) is nobody's to take.
	oldChain := auth.ChainID(newDevice(t).Public().(ed25519.PublicKey))
	if err := e.store.Create(t.Context(), oldChain, 0, func(store.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if rec := probe(bob, bobAccount, oldChain); rec.Code != http.StatusForbidden {
		t.Fatalf("claim of an old chain: HTTP %d", rec.Code)
	}
}

func TestDeleteAccount(t *testing.T) {
	e := newEnv(t, Config{})
	device, other := newDevice(t), newDevice(t)
	account := e.create(id(1), device)
	keep := e.create(id(2), other)
	for _, chainID := range []string{id(0xa1), id(0xa2)} {
		e.do("GET", "/v3/chains/"+chainID+"/probe", token(device, account), nil)
	}
	e.do("GET", "/v3/chains/"+id(0xb1)+"/probe", token(other, keep), nil)
	// The chains exist: a device synced.
	for _, chainID := range []string{id(0xa1), id(0xb1)} {
		if err := e.store.Create(t.Context(), chainID, 0, func(store.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}

	if rec := e.do("DELETE", "/v3/account", token(device, account), authRequest{Auth: bytes.Repeat([]byte{4}, 32)}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("delete with a wrong password: HTTP %d", rec.Code)
	}
	if rec := e.do("DELETE", "/v3/account", token(device, account), authRequest{Auth: bytes.Repeat([]byte{3}, 32)}); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: HTTP %d: %s", rec.Code, rec.Body)
	}
	if strings.Join(e.cleared, ",") != id(0xa1) {
		t.Fatalf("cleared %v, want the one chain that existed", e.cleared)
	}
	if _, ok, _ := e.store.Chain(t.Context(), id(0xa1)); ok {
		t.Fatal("the account's chain is still there")
	}
	if _, ok, _ := e.store.Chain(t.Context(), id(0xb1)); !ok {
		t.Fatal("another account's chain went too")
	}
	if rec := e.do("GET", "/v3/devices", token(device, account), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("device of a deleted account: HTTP %d", rec.Code)
	}
	// The words are free for a new account.
	e.create(id(1), newDevice(t))
}
