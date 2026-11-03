// Package server is the HTTP interface of the sync server:
//
//	POST /v2/command/   Chromium sync protocol (protobuf), authenticated
//	GET  /v2/events     Server-Sent Events of a chain, authenticated
//	GET  /health        200 when the database answers
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
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

const (
	// maxRequestBytes bounds a request body after decompression.
	maxRequestBytes = 32 << 20
	// commandTimeout bounds the work on one protocol request.
	commandTimeout = time.Minute
	// minGzipBytes is the smallest response worth compressing.
	minGzipBytes = 1024
	// queueTimeout is how long a request waits for a free slot.
	queueTimeout = 10 * time.Second
)

// Config is what the handler needs.
type Config struct {
	Service *protocol.Service
	Events  http.Handler
	Limiter *ratelimit.Limiter
	Health  func(context.Context) error
	Log     *slog.Logger
	// MaxConcurrent bounds the protocol requests handled at once, which
	// bounds memory; 0 for no limit.
	MaxConcurrent int
}

// Handler returns the server's routes.
func Handler(c Config) http.Handler {
	h := &handler{Config: c}
	if c.MaxConcurrent > 0 {
		h.slots = make(chan struct{}, c.MaxConcurrent)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v2/command/{$}", auth.Middleware(http.HandlerFunc(h.command)))
	mux.Handle("POST /v2/command", auth.Middleware(http.HandlerFunc(h.command)))
	mux.Handle("GET /v2/events", auth.Middleware(h.limit(c.Events)))
	mux.HandleFunc("GET /health", h.health)
	return mux
}

type handler struct {
	Config
	slots chan struct{} // nil: no limit
}

func (h *handler) command(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), commandTimeout)
	defer cancel()
	rc := http.NewResponseController(w)
	// Not every ResponseWriter supports deadlines; the context still ends.
	_ = rc.SetReadDeadline(time.Now().Add(commandTimeout))
	_ = rc.SetWriteDeadline(time.Now().Add(commandTimeout))
	chainID := auth.ChainFromContext(ctx)

	// Refuse early, before reading what may be a large body.
	if ok, wait := h.Limiter.Allow(chainID); !ok {
		h.respond(w, r, protocol.Throttled(wait))
		return
	}
	if h.slots != nil {
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		case <-time.After(queueTimeout):
			w.Header().Set("Retry-After", "30")
			http.Error(w, "server busy", http.StatusServiceUnavailable)
			return
		case <-ctx.Done():
			return
		}
	}

	body, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msg := &pb.ClientToServerMessage{}
	if err := proto.Unmarshal(body, msg); err != nil {
		http.Error(w, "malformed ClientToServerMessage", http.StatusBadRequest)
		return
	}

	rsp, err := h.Service.Handle(ctx, chainID, msg)
	if err != nil {
		h.Log.Error("sync request failed", "chain", chainID,
			"contents", msg.GetMessageContents().String(), "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.respond(w, r, rsp)
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

// limit refuses requests of chains over the rate limit with 429.
func (h *handler) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := h.Limiter.Allow(auth.ChainFromContext(r.Context())); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(max(wait, time.Second)/time.Second)))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
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

// readBody reads a request body, compressed with gzip if the client says so.
func readBody(r *http.Request) ([]byte, error) {
	body := io.Reader(r.Body)
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, errors.New("malformed gzip body")
		}
		defer gz.Close()
		body = gz
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
	data, err := io.ReadAll(io.LimitReader(body, maxRequestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxRequestBytes {
		return nil, fmt.Errorf("request is larger than %d bytes", maxRequestBytes)
	}
	return data, nil
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
