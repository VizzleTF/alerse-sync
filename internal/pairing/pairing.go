// Package pairing relays the pairing of a new device with one that is
// already in a sync chain, so that the new one gets the chain's secret
// without anyone typing the 24 words:
//
//	POST   /v2/pair               token, {"slot","msg"}            existing device opens a slot
//	GET    /v2/pair/{slot}        -> {"msg","joined","bmsg","name","confirm","box"}
//	POST   /v2/pair/{slot}/join   {"msg","name","confirm"}         new device answers, once
//	POST   /v2/pair/{slot}/approve token, {"box"}                  existing device sends the secret
//	DELETE /v2/pair/{slot}        token                            existing device cancels
//
// The two devices run SPAKE2 keyed by a short one-time code that only the
// user sees, and the secret travels encrypted with the key they agree on.
// The server is a dumb relay: it sees PAKE messages and ciphertext, never
// the code. Slots last TTL. docs/protocol.md describes the protocol.
package pairing

import (
	"errors"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/jsonapi"
	"github.com/VizzleTF/arcomium-sync/internal/metrics"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	"github.com/VizzleTF/arcomium-sync/internal/store"
)

// TTL is how long a pairing slot lives.
const TTL = 5 * time.Minute

const (
	msgSize     = 32 // SPAKE2 over Curve25519
	confirmSize = 32 // HMAC-SHA256
	maxBox      = 256
	maxName     = 64
)

// Config is what the handler needs.
type Config struct {
	Store store.PairStore
	// Opens limits new slots per chain, Joins join attempts per client
	// address, Reads slot reads per client address; nil for no limit.
	Opens *ratelimit.Limiter
	Joins *ratelimit.Limiter
	Reads *ratelimit.Limiter
	Log   *slog.Logger
	// Metrics may be nil.
	Metrics *metrics.Metrics
	// TTL overrides TTL in tests.
	TTL time.Duration
}

type handler struct{ Config }

// Handler returns the pairing routes, under /v2/pair.
func Handler(c Config) http.Handler {
	if c.TTL == 0 {
		c.TTL = TTL
	}
	h := &handler{c}
	mux := http.NewServeMux()
	mux.Handle("POST /v2/pair", auth.Middleware(http.HandlerFunc(h.open)))
	mux.HandleFunc("GET /v2/pair/{slot}", h.get)
	mux.HandleFunc("POST /v2/pair/{slot}/join", h.join)
	mux.Handle("POST /v2/pair/{slot}/approve", auth.Middleware(http.HandlerFunc(h.approve)))
	mux.Handle("DELETE /v2/pair/{slot}", auth.Middleware(http.HandlerFunc(h.cancel)))
	return mux
}

type openRequest struct {
	Slot string `json:"slot"`
	Msg  []byte `json:"msg"`
}

type joinRequest struct {
	Msg     []byte `json:"msg"`
	Name    string `json:"name"`
	Confirm []byte `json:"confirm"`
}

type approveRequest struct {
	Box []byte `json:"box"`
}

type slotResponse struct {
	Msg     []byte `json:"msg"`
	Joined  bool   `json:"joined"`
	BMsg    []byte `json:"bmsg,omitempty"`
	Name    string `json:"name,omitempty"`
	Confirm []byte `json:"confirm,omitempty"`
	Box     []byte `json:"box,omitempty"`
	// Expires is the number of seconds the slot has left.
	Expires int `json:"expires"`
}

func (h *handler) open(w http.ResponseWriter, r *http.Request) {
	chainID := auth.ChainFromContext(r.Context())
	if !h.allow(w, h.Opens, chainID) {
		return
	}
	var req openRequest
	if !jsonapi.Read(w, r, &req) || !jsonapi.ValidID(w, req.Slot) {
		return
	}
	if len(req.Msg) != msgSize {
		jsonapi.Error(w, http.StatusBadRequest, "msg must be 32 bytes")
		return
	}
	err := h.Store.CreatePairSlot(r.Context(), store.PairSlot{ID: req.Slot, ChainID: chainID, AMsg: req.Msg}, h.TTL)
	switch {
	case errors.Is(err, store.ErrSlotTaken):
		jsonapi.Error(w, http.StatusConflict, "the slot is taken; make a new code")
	case errors.Is(err, store.ErrNoChain):
		jsonapi.Error(w, http.StatusConflict, "the sync chain is not on this server yet")
	case err != nil:
		h.fail(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	if !h.allow(w, h.Reads, jsonapi.AddrKey(r)) {
		return
	}
	id := r.PathValue("slot")
	if !jsonapi.ValidID(w, id) {
		return
	}
	slot, ok, err := h.Store.PairSlot(r.Context(), id, h.TTL)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !ok {
		jsonapi.Error(w, http.StatusNotFound, "no such pairing code, or it expired")
		return
	}
	jsonapi.Write(w, http.StatusOK, slotResponse{
		Msg: slot.AMsg, Joined: slot.BMsg != nil, BMsg: slot.BMsg, Name: slot.BName,
		Confirm: slot.BConfirm, Box: slot.Box,
		Expires: int(max(0, time.Until(slot.Created.Add(h.TTL)).Seconds())),
	})
}

func (h *handler) join(w http.ResponseWriter, r *http.Request) {
	if !h.allow(w, h.Joins, jsonapi.AddrKey(r)) {
		return
	}
	id := r.PathValue("slot")
	var req joinRequest
	if !jsonapi.ValidID(w, id) || !jsonapi.Read(w, r, &req) {
		return
	}
	switch {
	case len(req.Msg) != msgSize:
		jsonapi.Error(w, http.StatusBadRequest, "msg must be 32 bytes")
		return
	case len(req.Confirm) != confirmSize:
		jsonapi.Error(w, http.StatusBadRequest, "confirm must be 32 bytes")
		return
	case req.Name == "" || !utf8.ValidString(req.Name) || utf8.RuneCountInString(req.Name) > maxName:
		jsonapi.Error(w, http.StatusBadRequest, "name must be 1 to 64 characters")
		return
	}
	h.done(w, h.Store.JoinPairSlot(r.Context(), id, req.Msg, req.Name, req.Confirm, h.TTL))
}

func (h *handler) approve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("slot")
	var req approveRequest
	if !jsonapi.ValidID(w, id) || !jsonapi.Read(w, r, &req) {
		return
	}
	if len(req.Box) == 0 || len(req.Box) > maxBox {
		jsonapi.Error(w, http.StatusBadRequest, "box must be 1 to 256 bytes")
		return
	}
	h.done(w, h.Store.ApprovePairSlot(r.Context(), id, auth.ChainFromContext(r.Context()), req.Box, h.TTL))
}

func (h *handler) cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("slot")
	if !jsonapi.ValidID(w, id) {
		return
	}
	h.done(w, h.Store.DeletePairSlot(r.Context(), id, auth.ChainFromContext(r.Context())))
}

func (h *handler) done(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNoSlot):
		jsonapi.Error(w, http.StatusNotFound, "no such pairing code, or it expired")
	case errors.Is(err, store.ErrSlotUsed):
		jsonapi.Error(w, http.StatusConflict, "the pairing code was used already")
	case err != nil:
		h.fail(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *handler) allow(w http.ResponseWriter, l *ratelimit.Limiter, key string) bool {
	if key == "" {
		return true
	}
	if ok, wait := l.Allow(key); !ok {
		h.Metrics.Limited("pair_rate")
		jsonapi.TooMany(w, wait)
		return false
	}
	return true
}

func (h *handler) fail(w http.ResponseWriter, err error) {
	h.Log.Error("pairing request failed", "error", err)
	jsonapi.Error(w, http.StatusInternalServerError, "internal error")
}
