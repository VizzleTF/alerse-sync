// Package account serves accounts (/v3): a user's devices, each with an
// Ed25519 key of its own, the chains they sync, and the account key in an
// envelope the server cannot open.
//
//	POST   /v3/unlock/params  {"id"}                                    -> {"scheme","salt","kdf"}
//	POST   /v3/account        {"id","scheme","salt","kdf","auth","wrapped","device"} -> 201 {"account"}
//	POST   /v3/unlock         {"id","auth","device"}                    -> {"account","wrapped"}
//	GET    /v3/account        device token                              -> {"account","kind","created"}
//	DELETE /v3/account        device token, {"auth"}
//	GET    /v3/devices        device token                              -> {"devices":[...]}
//	DELETE /v3/devices/{id}   device token, {"auth"}
//
// The scheme of this server is sk-v1: the client derives the envelope's id
// and its keys from the 24 words (a 256-bit secret key) and a password.
// Requests of the account's chains, /v3/chains/{chain}/command/ and
// /v3/chains/{chain}/events, go through Authenticate and ChainAccess.
// docs/protocol.md describes the scheme and its limits.
package account

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/credential"
	"github.com/VizzleTF/arcomium-sync/internal/jsonapi"
	"github.com/VizzleTF/arcomium-sync/internal/metrics"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	"github.com/VizzleTF/arcomium-sync/internal/store"
)

// SchemeSK is the envelope scheme of accounts opened with the 24 words and a
// password: scrypt over the password, mixed with HKDF of the secret key,
// HKDF-SHA256 to an authentication and a wrapping key.
const SchemeSK = "sk-v1"

const (
	authSize    = 32
	minSaltSize = 16
	maxSaltSize = 64
	minWrapped  = 16
	maxWrapped  = 512
	maxName     = 64
	// fakeSecret names the server secret that makes up salts and verifiers
	// for envelope ids that do not exist.
	fakeSecret = "account-fake-salt" //nolint:gosec // the name of the secret, not its value
	// TouchInterval is how often a device's last contact is recorded.
	TouchInterval = 5 * time.Minute
)

// Config is what the handler needs.
type Config struct {
	Store store.AccountStore
	// Hasher hashes authentication keys; nil makes one of Hash.
	Hasher *credential.Hasher
	Hash   credential.Hash
	// Creates limits new accounts per client address, Lookups params
	// requests per client address, Attempts password checks per envelope,
	// AddrAttempts per client address, Changes requests per account. nil
	// for no limit.
	Creates      *ratelimit.Limiter
	Lookups      *ratelimit.Limiter
	Attempts     *ratelimit.Limiter
	AddrAttempts *ratelimit.Limiter
	Changes      *ratelimit.Limiter
	// MaxAccounts, MaxDevices (active, per account) and MaxChains (per
	// account) bound what the server keeps; 0 for no limit.
	MaxAccounts int64
	MaxDevices  int
	MaxChains   int
	// Cleared, if set, tells a removed chain's devices that it is gone.
	Cleared func(ctx context.Context, chainID string)
	Log     *slog.Logger
	Metrics *metrics.Metrics
}

// Accounts serves the account API and checks device tokens.
type Accounts struct {
	Config
	fakes *credential.Fakes
}

// New returns the account service.
func New(c Config) *Accounts {
	if c.Hasher == nil {
		c.Hasher = credential.NewHasher(c.Hash)
	}
	return &Accounts{Config: c, fakes: &credential.Fakes{Secret: func(ctx context.Context) ([]byte, error) {
		return c.Store.ServerSecret(ctx, fakeSecret)
	}}}
}

// Handler returns the account routes, under /v3/account, /v3/unlock and
// /v3/devices.
func (a *Accounts) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v3/unlock/params", a.params)
	mux.HandleFunc("POST /v3/unlock", a.unlock)
	mux.HandleFunc("POST /v3/account", a.create)
	mux.Handle("GET /v3/account", a.Authenticate(http.HandlerFunc(a.get)))
	mux.Handle("DELETE /v3/account", a.Authenticate(http.HandlerFunc(a.remove)))
	mux.Handle("GET /v3/devices", a.Authenticate(http.HandlerFunc(a.devices)))
	mux.Handle("DELETE /v3/devices/{id}", a.Authenticate(http.HandlerFunc(a.removeDevice)))
	return mux
}

type deviceInfo struct {
	PublicKey []byte `json:"public_key"`
	Name      string `json:"name"`
}

type paramsRequest struct {
	ID string `json:"id"`
}

type paramsResponse struct {
	Scheme string         `json:"scheme"`
	Salt   []byte         `json:"salt"`
	KDF    credential.KDF `json:"kdf"`
}

type createRequest struct {
	ID      string         `json:"id"`
	Scheme  string         `json:"scheme"`
	Salt    []byte         `json:"salt"`
	KDF     credential.KDF `json:"kdf"`
	Auth    []byte         `json:"auth"`
	Wrapped []byte         `json:"wrapped"`
	Device  deviceInfo     `json:"device"`
}

type createResponse struct {
	Account string `json:"account"`
}

type unlockRequest struct {
	ID     string     `json:"id"`
	Auth   []byte     `json:"auth"`
	Device deviceInfo `json:"device"`
}

type unlockResponse struct {
	Account string `json:"account"`
	Wrapped []byte `json:"wrapped"`
}

type authRequest struct {
	Auth []byte `json:"auth"`
}

type accountResponse struct {
	Account string    `json:"account"`
	Kind    string    `json:"kind"`
	Created time.Time `json:"created"`
}

type deviceResponse struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
	Current  bool      `json:"current"`
}

type devicesResponse struct {
	Devices []deviceResponse `json:"devices"`
}

func (a *Accounts) params(w http.ResponseWriter, r *http.Request) {
	if !a.allow(w, a.Lookups, jsonapi.AddrKey(r)) {
		return
	}
	var req paramsRequest
	if !jsonapi.Read(w, r, &req) || !jsonapi.ValidID(w, req.ID) {
		return
	}
	e, ok, err := a.Store.Envelope(r.Context(), req.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	rsp := paramsResponse{Scheme: SchemeSK, KDF: credential.DefaultKDF}
	if ok {
		rsp.Scheme, rsp.Salt = e.Scheme, e.Salt
		if err := json.Unmarshal(e.KDF, &rsp.KDF); err != nil {
			a.fail(w, err)
			return
		}
	} else {
		// An id without an account gets a salt that looks real and stays
		// the same, so the answer does not tell whether it has one.
		if rsp.Salt, err = a.fakes.Value(r.Context(), "salt", req.ID); err != nil {
			a.fail(w, err)
			return
		}
	}
	jsonapi.Write(w, http.StatusOK, rsp)
}

func (a *Accounts) create(w http.ResponseWriter, r *http.Request) {
	if !a.allow(w, a.Creates, jsonapi.AddrKey(r)) {
		return
	}
	var req createRequest
	if !jsonapi.Read(w, r, &req) || !jsonapi.ValidID(w, req.ID) {
		return
	}
	switch {
	case req.Scheme != SchemeSK:
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
	case len(req.Wrapped) < minWrapped || len(req.Wrapped) > maxWrapped:
		jsonapi.Error(w, http.StatusBadRequest, "wrapped must be 16 to 512 bytes")
		return
	case !validDevice(w, req.Device):
		return
	}
	if a.MaxAccounts > 0 {
		n, err := a.Store.CountAccounts(r.Context())
		if err != nil {
			a.fail(w, err)
			return
		}
		if n >= a.MaxAccounts {
			a.Metrics.Limited("max_accounts")
			jsonapi.Error(w, http.StatusServiceUnavailable, "the server has no room for another account")
			return
		}
	}

	verifierSalt := make([]byte, 16)
	_, _ = rand.Read(verifierSalt) // never fails
	verifier, err := a.Hasher.Sum(r.Context(), req.Auth, verifierSalt)
	if err != nil {
		return // the client went away
	}
	kdf, _ := json.Marshal(req.KDF)
	id := make([]byte, auth.AccountSize)
	_, _ = rand.Read(id)
	account := store.Account{ID: hex.EncodeToString(id), Kind: store.AccountKindSK}
	err = a.Store.CreateAccount(r.Context(), account,
		store.Envelope{
			ID: req.ID, Scheme: req.Scheme, Salt: req.Salt, KDF: kdf,
			Verifier: verifier, VerifierSalt: verifierSalt, Wrapped: req.Wrapped,
		},
		store.Device{PublicKey: req.Device.PublicKey, Name: req.Device.Name})
	switch {
	case errors.Is(err, store.ErrEnvelopeTaken):
		// Only whoever holds the 24 words can derive the id, so this tells
		// nobody else anything.
		jsonapi.Error(w, http.StatusConflict, "an account with these words exists; sign in instead")
	case err != nil:
		a.fail(w, err)
	default:
		jsonapi.Write(w, http.StatusCreated, createResponse{Account: account.ID})
	}
}

func (a *Accounts) unlock(w http.ResponseWriter, r *http.Request) {
	var req unlockRequest
	if !jsonapi.Read(w, r, &req) || !jsonapi.ValidID(w, req.ID) {
		return
	}
	if !a.allow(w, a.AddrAttempts, jsonapi.AddrKey(r)) || !a.allow(w, a.Attempts, req.ID) {
		return
	}
	if len(req.Auth) != authSize {
		jsonapi.Error(w, http.StatusBadRequest, "auth must be 32 bytes")
		return
	}
	if !validDevice(w, req.Device) {
		return
	}
	e, ok, err := a.Store.Envelope(r.Context(), req.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	if !a.check(w, r, req.ID, e, ok, req.Auth) {
		return
	}
	err = a.Store.AddDevice(r.Context(), store.Device{
		AccountID: e.AccountID, PublicKey: req.Device.PublicKey, Name: req.Device.Name,
	}, a.MaxDevices)
	switch {
	case errors.Is(err, store.ErrDeviceRemoved):
		jsonapi.Error(w, http.StatusConflict, "this device key was removed from the account; make a new one")
	case errors.Is(err, store.ErrTooManyDevices):
		jsonapi.Error(w, http.StatusConflict, "the account has too many devices; remove one first")
	case errors.Is(err, store.ErrNoAccount):
		jsonapi.Error(w, http.StatusUnauthorized, "wrong words or password")
	case err != nil:
		a.fail(w, err)
	default:
		jsonapi.Write(w, http.StatusOK, unlockResponse{Account: e.AccountID, Wrapped: e.Wrapped})
	}
}

// check compares key with the envelope id, e, or answers 401. An envelope
// that does not exist (ok false) costs the same work, so the time does not
// tell.
func (a *Accounts) check(w http.ResponseWriter, r *http.Request, id string, e store.Envelope, ok bool, key []byte) bool {
	verifier, salt := e.Verifier, e.VerifierSalt
	if !ok {
		var err error
		if verifier, err = a.fakes.Value(r.Context(), "verifier", id); err == nil {
			salt, err = a.fakes.Value(r.Context(), "verifier-salt", id)
		}
		if err != nil {
			a.fail(w, err)
			return false
		}
	}
	hash, err := a.Hasher.Sum(r.Context(), key, salt)
	if err != nil {
		return false // the client went away
	}
	if !ok || subtle.ConstantTimeCompare(hash, verifier) != 1 {
		a.Metrics.Limited("unlock_failed")
		jsonapi.Error(w, http.StatusUnauthorized, "wrong words or password")
		return false
	}
	return true
}

// confirm checks the password again before a change that a stolen device
// must not make: removing a device or the account.
func (a *Accounts) confirm(w http.ResponseWriter, r *http.Request, accountID string) bool {
	var req authRequest
	if !jsonapi.Read(w, r, &req) || !a.allow(w, a.AddrAttempts, jsonapi.AddrKey(r)) {
		return false
	}
	if len(req.Auth) != authSize {
		jsonapi.Error(w, http.StatusBadRequest, "auth must be 32 bytes")
		return false
	}
	e, ok, err := a.Store.AccountEnvelope(r.Context(), accountID, SchemeSK)
	if err != nil {
		a.fail(w, err)
		return false
	}
	if !ok {
		jsonapi.Error(w, http.StatusUnauthorized, "wrong words or password")
		return false
	}
	// The same limit as unlocks of the envelope: a device cannot guess the
	// password faster than a stranger.
	if !a.allow(w, a.Attempts, e.ID) {
		return false
	}
	return a.check(w, r, e.ID, e, true, req.Auth)
}

func (a *Accounts) get(w http.ResponseWriter, r *http.Request) {
	d, _ := auth.DeviceFromContext(r.Context())
	if !a.allow(w, a.Changes, d.Account) {
		return
	}
	account, ok, err := a.Store.Account(r.Context(), d.Account)
	if err != nil {
		a.fail(w, err)
		return
	}
	if !ok {
		jsonapi.Error(w, http.StatusNotFound, "no such account")
		return
	}
	jsonapi.Write(w, http.StatusOK, accountResponse{Account: account.ID, Kind: account.Kind, Created: account.CreatedAt.UTC()})
}

func (a *Accounts) remove(w http.ResponseWriter, r *http.Request) {
	d, _ := auth.DeviceFromContext(r.Context())
	if !a.allow(w, a.Changes, d.Account) || !a.confirm(w, r, d.Account) {
		return
	}
	// The work does not stop when the client goes away: half an account
	// helps nobody. DeleteAccount runs in one transaction anyway.
	ctx := context.WithoutCancel(r.Context())
	chains, err := a.Store.DeleteAccount(ctx, d.Account)
	if err != nil {
		a.fail(w, err)
		return
	}
	if a.Cleared != nil {
		for _, chainID := range chains {
			a.Cleared(ctx, chainID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Accounts) devices(w http.ResponseWriter, r *http.Request) {
	d, _ := auth.DeviceFromContext(r.Context())
	if !a.allow(w, a.Changes, d.Account) {
		return
	}
	devices, err := a.Store.Devices(r.Context(), d.Account)
	if err != nil {
		a.fail(w, err)
		return
	}
	rsp := devicesResponse{Devices: make([]deviceResponse, 0, len(devices))}
	for _, device := range devices {
		rsp.Devices = append(rsp.Devices, deviceResponse{
			ID: hex.EncodeToString(device.PublicKey), Name: device.Name,
			Created: device.CreatedAt.UTC(), LastSeen: device.LastSeen.UTC(),
			Current: subtle.ConstantTimeCompare(device.PublicKey, d.PublicKey) == 1,
		})
	}
	jsonapi.Write(w, http.StatusOK, rsp)
}

func (a *Accounts) removeDevice(w http.ResponseWriter, r *http.Request) {
	d, _ := auth.DeviceFromContext(r.Context())
	id := r.PathValue("id")
	if !a.allow(w, a.Changes, d.Account) || !jsonapi.ValidID(w, id) {
		return
	}
	if !a.confirm(w, r, d.Account) {
		return
	}
	target, _ := hex.DecodeString(id)
	err := a.Store.RemoveDevice(r.Context(), d.Account, target)
	switch {
	case errors.Is(err, store.ErrNoDevice):
		jsonapi.Error(w, http.StatusNotFound, "no such device")
	case errors.Is(err, store.ErrLastDevice):
		jsonapi.Error(w, http.StatusConflict, "the account's only device cannot be removed; delete the account instead")
	case err != nil:
		a.fail(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// Authenticate rejects requests without a valid device token of an active
// device with 401, and stores the device in the request context.
func (a *Accounts) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		d, err := auth.VerifyDevice(token, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		active, err := a.Store.UseDevice(r.Context(), d.Account, d.PublicKey, TouchInterval)
		if err != nil {
			a.Log.Error("look up device", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !active {
			// Removed, never added, or its account was deleted.
			http.Error(w, "the device is not signed in to the account", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithDevice(r.Context(), d)))
	})
}

// ChainAccess lets an authenticated device reach the chain named by the
// request's {chain} path value, if the chain is its account's. A chain that
// does not exist yet becomes the account's on first use; a chain of
// another account, or an existing chain of no account, gets 403. It stores
// the chain as auth.Middleware does, for the handlers of /v2.
func (a *Accounts) ChainAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, _ := auth.DeviceFromContext(r.Context())
		chainID := r.PathValue("chain")
		if !validChainID(chainID) {
			http.Error(w, "chain must be 64 lowercase hex digits", http.StatusBadRequest)
			return
		}
		owner, claimed, err := a.Store.ChainAccount(r.Context(), chainID)
		if err == nil && !claimed {
			err = a.Store.ClaimChain(r.Context(), d.Account, chainID, a.MaxChains)
			owner = d.Account
		}
		switch {
		case errors.Is(err, store.ErrChainTaken), err == nil && owner != d.Account:
			http.Error(w, "the sync chain is not the account's", http.StatusForbidden)
			return
		case errors.Is(err, store.ErrTooManyChains):
			a.Metrics.Limited("account_chains")
			http.Error(w, "the account has too many sync chains", http.StatusForbidden)
			return
		case errors.Is(err, store.ErrNoAccount):
			http.Error(w, "the device is not signed in to the account", http.StatusUnauthorized)
			return
		case err != nil:
			a.Log.Error("claim sync chain", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithChain(r.Context(), chainID)))
	})
}

// RefuseAccountChains refuses chain tokens (/v2) of chains that belong to
// an account with 403: every device of the account knows the chain's key,
// a removed one too, so only device tokens may reach such a chain. It takes
// the chain auth.Middleware stored, or else checks the request's token
// itself; requests without a valid chain token pass, for the handlers
// behind to check.
func (a *Accounts) RefuseAccountChains(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chainID := auth.ChainFromContext(r.Context())
		if chainID == "" {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			var err error
			if chainID, err = auth.Verify(token, time.Now()); err != nil {
				next.ServeHTTP(w, r)
				return
			}
		}
		_, owned, err := a.Store.ChainAccount(r.Context(), chainID)
		if err != nil {
			a.Log.Error("look up chain account", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if owned {
			http.Error(w, "the sync chain belongs to an account; use /v3", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validChainID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == id
}

func validDevice(w http.ResponseWriter, d deviceInfo) bool {
	switch {
	case len(d.PublicKey) != ed25519.PublicKeySize:
		jsonapi.Error(w, http.StatusBadRequest, "device public_key must be 32 bytes")
		return false
	case d.Name == "" || !utf8.ValidString(d.Name) || utf8.RuneCountInString(d.Name) > maxName ||
		strings.ContainsFunc(d.Name, unicode.IsControl):
		jsonapi.Error(w, http.StatusBadRequest, "device name must be 1 to 64 characters")
		return false
	}
	return true
}

func (a *Accounts) allow(w http.ResponseWriter, l *ratelimit.Limiter, key string) bool {
	if key == "" {
		return true
	}
	if ok, wait := l.Allow(key); !ok {
		a.Metrics.Limited("account_rate")
		jsonapi.TooMany(w, wait)
		return false
	}
	return true
}

func (a *Accounts) fail(w http.ResponseWriter, err error) {
	a.Log.Error("account request failed", "error", err)
	jsonapi.Error(w, http.StatusInternalServerError, "internal error")
}
