// Package login lets a device join a sync chain with a login name and a
// password instead of the chain's 24-word sync code. It is optional and the
// server stays zero-knowledge:
//
//	POST   /v2/login/params  {"id"}                  -> {"scheme","salt","kdf"}
//	POST   /v2/login/unlock  {"id","auth"}           -> {"secret"}
//	GET    /v2/login         token                   -> {"id","scheme","kdf","updated"}
//	PUT    /v2/login         token, {"id","scheme","salt","kdf","auth","secret"}
//	DELETE /v2/login         token
//
// The client derives two keys from the login name and password with a
// memory-hard KDF: an authentication key, which it sends, and an encryption
// key, which never leaves it and wraps the chain's secret. The server knows
// the login name only by a hash (id), stores a slow hash of the
// authentication key, and hands the wrapped secret to whoever proves the key.
// docs/protocol.md describes the scheme and its limits.
package login

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/credential"
	"github.com/VizzleTF/arcomium-sync/internal/jsonapi"
	"github.com/VizzleTF/arcomium-sync/internal/metrics"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	"github.com/VizzleTF/arcomium-sync/internal/store"
)

// Scheme is the client scheme this server knows: scrypt over the password,
// HKDF-SHA256 to an authentication and an encryption key, and AES-256-GCM
// around the chain's secret. A record names its scheme, so another one, such
// as OPAQUE, can be added next to it.
const Scheme = "pw-v1"

// KDF is the client's key derivation, with its parameters.
type KDF = credential.KDF

// DefaultKDF is what the server answers for login names without a login,
// and what clients use today: scrypt with 128 MiB.
var DefaultKDF = credential.DefaultKDF

// Hash is the Argon2id cost of the server's hash of authentication keys.
type Hash = credential.Hash

// DefaultHash is OWASP's minimum for Argon2id: 19 MiB, 2 passes.
var DefaultHash = credential.DefaultHash

const (
	authSize    = 32
	minSaltSize = 16
	maxSaltSize = 64
	minSecret   = 16
	maxSecret   = 512
	// fakeSecret names the server secret that makes up salts and
	// verifiers for login names without a login.
	fakeSecret = "login-fake-salt" //nolint:gosec // the name of the secret, not its value
)

// Config is what the handler needs.
type Config struct {
	Store store.LoginStore
	// EnsureChain creates the chain of a new login if no device has synced
	// yet, under the same limits as a first GetUpdates.
	EnsureChain func(ctx context.Context, chainID string) error
	// Attempts limits unlock attempts per login ID, AddrAttempts per client
	// address, Lookups params requests per client address, Changes
	// changes per chain. nil for no limit.
	Attempts     *ratelimit.Limiter
	AddrAttempts *ratelimit.Limiter
	Lookups      *ratelimit.Limiter
	Changes      *ratelimit.Limiter
	// Hasher hashes authentication keys; nil makes one of Hash.
	Hasher  *credential.Hasher
	Hash    Hash
	Log     *slog.Logger
	Metrics *metrics.Metrics
}

type handler struct {
	Config
	fakes *credential.Fakes
}

// Handler returns the login routes, under /v2/login.
func Handler(c Config) http.Handler {
	if c.Hasher == nil {
		c.Hasher = credential.NewHasher(c.Hash)
	}
	h := &handler{Config: c, fakes: &credential.Fakes{Secret: func(ctx context.Context) ([]byte, error) {
		return c.Store.ServerSecret(ctx, fakeSecret)
	}}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/login/params", h.params)
	mux.HandleFunc("POST /v2/login/unlock", h.unlock)
	mux.Handle("GET /v2/login", auth.Middleware(http.HandlerFunc(h.get)))
	mux.Handle("PUT /v2/login", auth.Middleware(http.HandlerFunc(h.set)))
	mux.Handle("DELETE /v2/login", auth.Middleware(http.HandlerFunc(h.remove)))
	return mux
}

type paramsRequest struct {
	ID string `json:"id"`
}

type paramsResponse struct {
	Scheme string `json:"scheme"`
	Salt   []byte `json:"salt"`
	KDF    KDF    `json:"kdf"`
}

type unlockRequest struct {
	ID   string `json:"id"`
	Auth []byte `json:"auth"`
}

type unlockResponse struct {
	Secret []byte `json:"secret"`
}

type setRequest struct {
	ID     string `json:"id"`
	Scheme string `json:"scheme"`
	Salt   []byte `json:"salt"`
	KDF    KDF    `json:"kdf"`
	Auth   []byte `json:"auth"`
	Secret []byte `json:"secret"`
}

type getResponse struct {
	ID      string    `json:"id"`
	Scheme  string    `json:"scheme"`
	KDF     KDF       `json:"kdf"`
	Updated time.Time `json:"updated"`
}

func (h *handler) params(w http.ResponseWriter, r *http.Request) {
	if !h.allow(w, h.Lookups, jsonapi.AddrKey(r)) {
		return
	}
	var req paramsRequest
	if !jsonapi.Read(w, r, &req) || !jsonapi.ValidID(w, req.ID) {
		return
	}
	l, ok, err := h.Store.Login(r.Context(), req.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	rsp := paramsResponse{Scheme: Scheme, KDF: DefaultKDF}
	if ok {
		rsp.Scheme, rsp.Salt = l.Scheme, l.Salt
		if err := json.Unmarshal(l.KDF, &rsp.KDF); err != nil {
			h.fail(w, err)
			return
		}
	} else {
		// A login name without a login gets a salt that looks real and
		// stays the same, so the answer does not tell whether it has one.
		fake, err := h.fake(r.Context(), "salt", req.ID)
		if err != nil {
			h.fail(w, err)
			return
		}
		rsp.Salt = fake
	}
	jsonapi.Write(w, http.StatusOK, rsp)
}

func (h *handler) unlock(w http.ResponseWriter, r *http.Request) {
	var req unlockRequest
	if !jsonapi.Read(w, r, &req) || !jsonapi.ValidID(w, req.ID) {
		return
	}
	if !h.allow(w, h.AddrAttempts, jsonapi.AddrKey(r)) || !h.allow(w, h.Attempts, req.ID) {
		return
	}
	if len(req.Auth) != authSize {
		jsonapi.Error(w, http.StatusBadRequest, "auth must be 32 bytes")
		return
	}
	l, ok, err := h.Store.Login(r.Context(), req.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	verifier, salt := l.Verifier, l.VerifierSalt
	if !ok {
		// The same work as for a real login, so the time does not tell.
		if verifier, err = h.fake(r.Context(), "verifier", req.ID); err == nil {
			salt, err = h.fake(r.Context(), "verifier-salt", req.ID)
		}
		if err != nil {
			h.fail(w, err)
			return
		}
	}
	hash, err := h.hash(r.Context(), req.Auth, salt)
	if err != nil {
		return // the client went away
	}
	if !ok || subtle.ConstantTimeCompare(hash, verifier) != 1 {
		h.Metrics.Limited("login_failed")
		jsonapi.Error(w, http.StatusUnauthorized, "wrong login or password")
		return
	}
	jsonapi.Write(w, http.StatusOK, unlockResponse{Secret: l.Secret})
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	chainID := auth.ChainFromContext(r.Context())
	if !h.allow(w, h.Changes, chainID) {
		return
	}
	l, ok, err := h.Store.ChainLogin(r.Context(), chainID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !ok {
		jsonapi.Error(w, http.StatusNotFound, "the sync chain has no login")
		return
	}
	rsp := getResponse{ID: l.ID, Scheme: l.Scheme, Updated: l.UpdatedAt.UTC()}
	if err := json.Unmarshal(l.KDF, &rsp.KDF); err != nil {
		h.fail(w, err)
		return
	}
	jsonapi.Write(w, http.StatusOK, rsp)
}

func (h *handler) set(w http.ResponseWriter, r *http.Request) {
	chainID := auth.ChainFromContext(r.Context())
	if !h.allow(w, h.Changes, chainID) {
		return
	}
	var req setRequest
	if !jsonapi.Read(w, r, &req) || !jsonapi.ValidID(w, req.ID) {
		return
	}
	switch {
	case req.Scheme != Scheme:
		jsonapi.Error(w, http.StatusBadRequest, "unknown scheme")
		return
	case !req.KDF.Valid():
		jsonapi.Error(w, http.StatusBadRequest, "kdf parameters out of range")
		return
	case len(req.Salt) < minSaltSize || len(req.Salt) > maxSaltSize:
		jsonapi.Error(w, http.StatusBadRequest, "salt must be 16 to 64 bytes")
		return
	case len(req.Auth) != authSize:
		jsonapi.Error(w, http.StatusBadRequest, "auth must be 32 bytes")
		return
	case len(req.Secret) < minSecret || len(req.Secret) > maxSecret:
		jsonapi.Error(w, http.StatusBadRequest, "secret must be 16 to 512 bytes")
		return
	}

	if err := h.EnsureChain(r.Context(), chainID); err != nil {
		var wait protocol.WaitError
		switch {
		case errors.As(err, &wait):
			h.Metrics.Limited("new_chains")
			jsonapi.TooMany(w, wait.Wait)
		case errors.Is(err, protocol.ErrFull):
			jsonapi.Error(w, http.StatusServiceUnavailable, err.Error())
		default:
			h.fail(w, err)
		}
		return
	}

	verifierSalt := make([]byte, 16)
	_, _ = rand.Read(verifierSalt) // never fails
	verifier, err := h.hash(r.Context(), req.Auth, verifierSalt)
	if err != nil {
		return
	}
	kdf, _ := json.Marshal(req.KDF)
	err = h.Store.SetLogin(r.Context(), store.Login{
		ID: req.ID, ChainID: chainID, Scheme: req.Scheme, Salt: req.Salt, KDF: kdf,
		Verifier: verifier, VerifierSalt: verifierSalt, Secret: req.Secret,
	})
	switch {
	case errors.Is(err, store.ErrLoginTaken):
		jsonapi.Error(w, http.StatusConflict, "this login is taken by another sync chain")
	case errors.Is(err, store.ErrNoChain):
		jsonapi.Error(w, http.StatusConflict, "the sync chain was removed")
	case err != nil:
		h.fail(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *handler) remove(w http.ResponseWriter, r *http.Request) {
	chainID := auth.ChainFromContext(r.Context())
	if !h.allow(w, h.Changes, chainID) {
		return
	}
	if err := h.Store.DeleteLogin(r.Context(), chainID); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) hash(ctx context.Context, key, salt []byte) ([]byte, error) {
	return h.Hasher.Sum(ctx, key, salt)
}

// fake makes up a value of kind for a login ID without a login, the same on
// every request and instance.
func (h *handler) fake(ctx context.Context, kind, id string) ([]byte, error) {
	return h.fakes.Value(ctx, kind, id)
}

func (h *handler) allow(w http.ResponseWriter, l *ratelimit.Limiter, key string) bool {
	if key == "" {
		return true
	}
	if ok, wait := l.Allow(key); !ok {
		h.Metrics.Limited("login_rate")
		jsonapi.TooMany(w, wait)
		return false
	}
	return true
}

func (h *handler) fail(w http.ResponseWriter, err error) {
	h.Log.Error("login request failed", "error", err)
	jsonapi.Error(w, http.StatusInternalServerError, "internal error")
}
