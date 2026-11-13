// Package auth checks the device tokens sent with every /v3 request
// (device.go) and carries the authenticated device and its sync chain in the
// request context.
//
// The server accepts a token from 24 hours before to 5 minutes after its own
// clock and keeps no state about it.
package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

const (
	// MaxAge is how long a token stays valid after it was issued.
	MaxAge = 24 * time.Hour
	// MaxSkew is how far in the future a token may be issued, to allow for
	// clock differences.
	MaxSkew = 5 * time.Minute
)

var (
	ErrMalformed = errors.New("malformed token")
	ErrSignature = errors.New("bad token signature")
	ErrExpired   = errors.New("token expired or issued in the future")
)

// ChainID returns the ID of the sync chain whose key is public, as clients
// derive it: hex(SHA-256(public key)).
func ChainID(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:])
}

type contextKey struct{}

// ChainFromContext returns the chain ID that WithChain stored.
func ChainFromContext(ctx context.Context) string {
	chainID, _ := ctx.Value(contextKey{}).(string)
	return chainID
}
