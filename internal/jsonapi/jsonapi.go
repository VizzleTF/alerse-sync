// Package jsonapi has the small helpers of the server's JSON endpoints
// (sign-in and pairing).
package jsonapi

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/clientip"
)

// MaxBodyBytes bounds a JSON request body.
const MaxBodyBytes = 4 << 10

// Read decodes a JSON body into v, or answers 400.
func Read(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes)).Decode(v); err != nil {
		Error(w, http.StatusBadRequest, "malformed request")
		return false
	}
	return true
}

// Write answers v as JSON, never cached.
func Write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error answers {"error": message}.
func Error(w http.ResponseWriter, status int, message string) {
	Write(w, status, map[string]string{"error": message})
}

// TooMany answers 429 with Retry-After.
func TooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(max(wait, time.Second)/time.Second)))
	Error(w, http.StatusTooManyRequests, "too many attempts; try again later")
}

// ValidID accepts 64 lowercase hex digits, or answers 400.
func ValidID(w http.ResponseWriter, id string) bool {
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != id {
		Error(w, http.StatusBadRequest, "id must be 64 lowercase hex digits")
		return false
	}
	return true
}

// AddrKey is the rate-limit key of the client address of r.
func AddrKey(r *http.Request) string {
	return clientip.Key(clientip.FromContext(r.Context()))
}
