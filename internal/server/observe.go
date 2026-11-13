package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/clientip"
)

// requestInfo is what the handlers learn about a request for its log entry
// and metrics.
type requestInfo struct {
	chain   string
	message string
	result  string
	bytesIn int
	// expected marks a refusal a well-behaved client gets too, which does
	// not count against its address.
	expected bool
}

type infoKey struct{}

func infoOf(ctx context.Context) *requestInfo {
	if info, ok := ctx.Value(infoKey{}).(*requestInfo); ok {
		return info
	}
	return &requestInfo{}
}

// recorder notes a response's status and size.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(data)
	r.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the connection, for deadlines
// and flushing.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// observe counts every request and, with AccessLog, logs it: never bodies or
// query strings, and the chain ID shortened.
func (h *handler) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &requestInfo{}
		rec := &recorder{ResponseWriter: w}
		start := time.Now()
		key := clientip.Key(clientip.FromContext(r.Context()))
		if banned, wait := h.Guard.Banned(key); banned {
			// Nothing else is done for a shut out address: no token check,
			// no body, no database.
			h.Metrics.Limited("banned")
			info.result = "BANNED"
			rec.Header().Set("Retry-After", strconv.Itoa(int(max(wait, time.Second)/time.Second)))
			http.Error(rec, "too many refused requests", http.StatusTooManyRequests)
		} else {
			next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
		}
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		if info.result != "BANNED" && refused(rec.status, info) && h.Guard.Fail(key) {
			h.Metrics.Limited("ban")
			h.Log.Warn("client address shut out", "addr", key)
		}

		route := routeOf(r.URL.Path)
		result := info.result
		if result == "" {
			result = strconv.Itoa(rec.status)
		}
		h.Metrics.Request(route, info.message, result)
		if !h.AccessLog {
			return
		}
		level := slog.LevelInfo
		if route == "probe" {
			level = slog.LevelDebug
		}
		chain := info.chain
		if len(chain) > 12 {
			chain = chain[:12]
		}
		h.Log.LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method), slog.String("path", logPath(r.URL.Path)),
			slog.Int("status", rec.status), slog.Duration("took", time.Since(start).Round(time.Microsecond)),
			slog.Int("bytes_in", info.bytesIn), slog.Int("bytes_out", rec.bytes),
			slog.String("chain", chain), slog.String("message", info.message), slog.String("result", info.result))
	})
}

// refused tells whether a response refuses something a client that follows
// the protocol does not send: a bad token, a malformed or oversized body, a
// path the server does not serve, a limit it was already told about.
func refused(status int, info *requestInfo) bool {
	if info.expected {
		return false
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusRequestTimeout,
		http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		return true
	}
	return info.result == "THROTTLED"
}

func routeOf(path string) string {
	switch path {
	case "/health", "/livez", "/readyz":
		return "probe"
	}
	if rest, ok := strings.CutPrefix(path, "/v3/chains/"); ok {
		switch _, route, _ := strings.Cut(rest, "/"); route {
		case "command", "command/":
			return "command"
		case "events":
			return "events"
		}
	}
	return "other"
}

// logPath shortens the chain and device IDs in a path, as the chain field of
// a log entry is.
func logPath(path string) string {
	for _, prefix := range []string{"/v3/chains/", "/v3/devices/"} {
		rest, ok := strings.CutPrefix(path, prefix)
		if !ok {
			continue
		}
		id, tail, slash := strings.Cut(rest, "/")
		if len(id) > 12 {
			id = id[:12]
		}
		if slash {
			tail = "/" + tail
		}
		return prefix + id + tail
	}
	return path
}
