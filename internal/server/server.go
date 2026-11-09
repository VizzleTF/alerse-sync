// Package server is the HTTP interface of the sync server:
//
//	POST /v2/command/   Chromium sync protocol (protobuf), authenticated
//	GET  /v2/events     Server-Sent Events of a chain, authenticated
//	/v2/login...        optional sign-in with a login name and password
//	                    (internal/login)
//	GET  /health        200 when the database answers
//	GET  /livez         200 while the process runs
//	GET  /readyz        200 when the server takes requests: the database
//	                    answers and it is not shutting down
package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/clientip"
	"github.com/VizzleTF/arcomium-sync/internal/metrics"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

const (
	// maxRequestBytes bounds a request body, before and after
	// decompression.
	maxRequestBytes = 32 << 20
	// commandTimeout bounds the work on one protocol request.
	commandTimeout = time.Minute
	// DefaultBodyTimeout is how long a client may take to send a request
	// body.
	DefaultBodyTimeout = 20 * time.Second
	// minGzipBytes is the smallest response worth compressing.
	minGzipBytes = 1024
	// queueTimeout is how long a request waits for a free slot.
	queueTimeout = 10 * time.Second
	// readersPerSlot is how many bodies may be read per processing slot.
	readersPerSlot = 4
)

// Config is what the handler needs.
type Config struct {
	Service *protocol.Service
	Events  http.Handler
	// Login serves /v2/login; nil leaves it out.
	Login http.Handler
	// Limiter limits requests per sync chain, AddrLimiter per client
	// address; nil for no limit.
	Limiter     *ratelimit.Limiter
	AddrLimiter *ratelimit.Limiter
	// ClientIP finds client addresses behind proxies; nil uses the
	// connection's address.
	ClientIP *clientip.Resolver
	// ChainExists, if set, refuses event streams of chains that do not
	// exist: only a GetUpdates creates a chain.
	ChainExists func(ctx context.Context, chainID string) (bool, error)
	// Health checks the database.
	Health func(context.Context) error
	// Ready, if set, reports false while the server shuts down.
	Ready func() bool
	Log   *slog.Logger
	// AccessLog logs every request, without bodies.
	AccessLog bool
	Metrics   *metrics.Metrics
	// MaxConcurrent bounds the protocol requests handled at once, which
	// bounds memory; 0 for no limit.
	MaxConcurrent int
	// BodyTimeout bounds the time to receive a request body;
	// DefaultBodyTimeout if 0.
	BodyTimeout time.Duration
}

// Handler returns the server's routes.
func Handler(c Config) http.Handler {
	h := &handler{Config: c}
	if h.BodyTimeout <= 0 {
		h.BodyTimeout = DefaultBodyTimeout
	}
	if c.MaxConcurrent > 0 {
		h.slots = make(chan struct{}, c.MaxConcurrent)
		h.readers = make(chan struct{}, c.MaxConcurrent*readersPerSlot)
	}
	command := h.limitAddr(true, auth.Middleware(h.named(http.HandlerFunc(h.command))))
	events := h.limitAddr(false, auth.Middleware(h.named(h.limitChain(h.requireChain(c.Events)))))
	mux := http.NewServeMux()
	mux.Handle("POST /v2/command/{$}", command)
	mux.Handle("POST /v2/command", command)
	mux.Handle("GET /v2/events", events)
	if c.Login != nil {
		login := h.limitAddr(false, c.Login)
		mux.Handle("/v2/login", login)
		mux.Handle("/v2/login/", login)
	}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /livez", h.livez)
	mux.HandleFunc("GET /readyz", h.readyz)
	return c.ClientIP.Middleware(h.observe(mux))
}

type handler struct {
	Config
	slots   chan struct{} // nil: no limit
	readers chan struct{} // nil: no limit
}

func (h *handler) command(w http.ResponseWriter, r *http.Request) {
	info := infoOf(r.Context())
	chainID := auth.ChainFromContext(r.Context())

	// Refuse early, before reading what may be a large body.
	if ok, wait := h.Limiter.Allow(chainID); !ok {
		h.Metrics.Limited("chain_rate")
		info.result = pb.SyncEnums_THROTTLED.String()
		h.respond(w, r, protocol.Throttled(wait))
		return
	}

	// The body is read before taking a slot, with a deadline of its own, so
	// clients that send slowly cannot hold the slots.
	rc := http.NewResponseController(w)
	// Not every ResponseWriter supports deadlines.
	_ = rc.SetReadDeadline(time.Now().Add(h.BodyTimeout))
	_ = rc.SetWriteDeadline(time.Now().Add(h.BodyTimeout + queueTimeout + commandTimeout))
	if !h.acquire(w, r, h.readers) {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	release(h.readers)
	if err != nil {
		h.bodyError(w, err)
		return
	}
	info.bytesIn = len(raw)
	// The server keeps reading the connection to notice a client that goes
	// away; a read deadline that passes would end the request's context.
	_ = rc.SetReadDeadline(time.Now().Add(queueTimeout + commandTimeout))

	if !h.acquire(w, r, h.slots) {
		return
	}
	defer release(h.slots)
	h.Metrics.Started()
	defer h.Metrics.Finished()

	body, err := decode(raw, r.Header.Get("Content-Encoding"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msg := &pb.ClientToServerMessage{}
	if err := proto.Unmarshal(body, msg); err != nil {
		http.Error(w, "malformed ClientToServerMessage", http.StatusBadRequest)
		return
	}
	info.message = messageName(msg)

	// The work does not stop when the client goes away or the server shuts
	// down: a commit half done helps nobody.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), commandTimeout)
	defer cancel()
	start := time.Now()
	rsp, err := h.Service.Handle(ctx, chainID, msg)
	if err != nil {
		h.Log.Error("sync request failed", "chain", chainID, "message", info.message, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.Metrics.Handled(info.message, time.Since(start), rsp)
	info.result = rsp.GetErrorCode().String()
	h.respond(w, r, rsp)
}

// acquire takes a slot of slots, or answers 503 if none frees up in time.
func (h *handler) acquire(w http.ResponseWriter, r *http.Request, slots chan struct{}) bool {
	if slots == nil {
		return true
	}
	select {
	case slots <- struct{}{}:
		return true
	default:
	}
	timer := time.NewTimer(queueTimeout)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return true
	case <-timer.C:
		h.Metrics.Limited("busy")
		w.Header().Set("Retry-After", "30")
		http.Error(w, "server busy", http.StatusServiceUnavailable)
	case <-r.Context().Done():
	}
	return false
}

func release(slots chan struct{}) {
	if slots != nil {
		<-slots
	}
}

func (h *handler) bodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		http.Error(w, fmt.Sprintf("request is larger than %d bytes", maxRequestBytes), http.StatusRequestEntityTooLarge)
	case errors.Is(err, os.ErrDeadlineExceeded):
		h.Metrics.Limited("slow_body")
		http.Error(w, "request body too slow", http.StatusRequestTimeout)
	default:
		http.Error(w, "read body", http.StatusBadRequest)
	}
}

func (h *handler) respond(w http.ResponseWriter, r *http.Request, rsp *pb.ClientToServerResponse) {
	data, err := proto.Marshal(rsp)
	if err != nil {
		h.Log.Error("encode sync response", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	writeBody(w, r, data)
}

// limitAddr refuses requests from client addresses over the rate limit,
// before their token is checked. Protocol requests get THROTTLED, which
// Chromium backs off on; others get 429.
func (h *handler) limitAddr(protocolResponse bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := clientip.Key(clientip.FromContext(r.Context()))
		if ok, wait := h.AddrLimiter.Allow(key); key != "" && !ok {
			h.Metrics.Limited("addr_rate")
			if protocolResponse {
				infoOf(r.Context()).result = pb.SyncEnums_THROTTLED.String()
				h.respond(w, r, protocol.Throttled(wait))
				return
			}
			w.Header().Set("Retry-After", strconv.Itoa(int(max(wait, time.Second)/time.Second)))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitChain refuses requests of chains over the rate limit with 429.
func (h *handler) limitChain(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := h.Limiter.Allow(auth.ChainFromContext(r.Context())); !ok {
			h.Metrics.Limited("chain_rate")
			w.Header().Set("Retry-After", strconv.Itoa(int(max(wait, time.Second)/time.Second)))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireChain refuses event streams of chains that do not exist, so that
// streams cost the server something only for chains in use.
func (h *handler) requireChain(next http.Handler) http.Handler {
	if h.ChainExists == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exists, err := h.ChainExists(r.Context(), auth.ChainFromContext(r.Context()))
		if err != nil {
			h.Log.Error("look up sync chain", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !exists {
			// The client connects again later, by when its first
			// GetUpdates has created the chain.
			w.Header().Set("Retry-After", "30")
			http.Error(w, "no such sync chain", http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// named puts the authenticated chain into the request's log entry.
func (h *handler) named(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		infoOf(r.Context()).chain = auth.ChainFromContext(r.Context())
		next.ServeHTTP(w, r)
	})
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.Health(ctx); err != nil {
		h.Log.Error("health check failed", "error", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

func (h *handler) livez(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, "ok")
}

func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	if h.Ready != nil && !h.Ready() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	h.health(w, r)
}

// decode returns a request body, decompressed if the client compressed it.
func decode(raw []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(encoding) {
	case "", "identity":
		return raw, nil
	case "gzip":
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, errors.New("malformed gzip body")
		}
		defer gz.Close()
		data, err := io.ReadAll(io.LimitReader(gz, maxRequestBytes+1))
		if err != nil {
			return nil, errors.New("malformed gzip body")
		}
		if len(data) > maxRequestBytes {
			return nil, fmt.Errorf("request is larger than %d bytes", maxRequestBytes)
		}
		return data, nil
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
}

// writeBody writes a response, compressed with gzip if the client accepts it
// and it is large enough to gain from it.
func writeBody(w http.ResponseWriter, r *http.Request, data []byte) {
	if len(data) >= minGzipBytes && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(data); err == nil && gz.Close() == nil {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Add("Vary", "Accept-Encoding")
			data = buf.Bytes()
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data) // fails only when the client went away
}

// messageName names a sync request for logs and metrics.
func messageName(msg *pb.ClientToServerMessage) string {
	return strings.ToLower(msg.GetMessageContents().String())
}
