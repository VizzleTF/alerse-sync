// Package auth checks the tokens devices send with every request and derives
// the sync chain they belong to.
//
// A sync chain has no account: every device of the chain derives the same
// Ed25519 key from the chain's sync code, so the key is the identity. A token
// proves the device holds the key:
//
//	token     = base64url(0x01 | public key (32) | issued at (8) | signature (64))
//	issued at = Unix time in milliseconds, big-endian
//	signature = Ed25519(private key, "arcomium-sync-token-v1" | public key | issued at)
//
// The server accepts a token from 24 hours before to 5 minutes after its own
// clock and keeps no state about it. The chain ID is hex(SHA-256(public key)).
package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	tokenVersion = 0x01
	tokenSize    = 1 + ed25519.PublicKeySize + 8 + ed25519.SignatureSize
	signContext  = "arcomium-sync-token-v1"

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

// NewToken issues a token for key at time now.
func NewToken(key ed25519.PrivateKey, now time.Time) string {
	public := key.Public().(ed25519.PublicKey)
	token := make([]byte, 0, tokenSize)
	token = append(token, tokenVersion)
	token = append(token, public...)
	token = binary.BigEndian.AppendUint64(token, uint64(now.UnixMilli()))
	token = append(token, ed25519.Sign(key, signedPart(token))...)
	return base64.RawURLEncoding.EncodeToString(token)
}

// Verify checks token at time now and returns the ID of its sync chain.
func Verify(token string, now time.Time) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != tokenSize || raw[0] != tokenVersion {
		return "", ErrMalformed
	}
	public := ed25519.PublicKey(raw[1 : 1+ed25519.PublicKeySize])
	issuedAt := time.UnixMilli(int64(binary.BigEndian.Uint64(raw[1+ed25519.PublicKeySize:])))
	signature := raw[tokenSize-ed25519.SignatureSize:]
	// The time first: it is cheap, and a flood of old tokens should not
	// cost a signature check each.
	if issuedAt.Before(now.Add(-MaxAge)) || issuedAt.After(now.Add(MaxSkew)) {
		return "", ErrExpired
	}
	if !ed25519.Verify(public, signedPart(raw[:tokenSize-ed25519.SignatureSize]), signature) {
		return "", ErrSignature
	}
	return ChainID(public), nil
}

// ChainID returns the ID of the sync chain of public key.
func ChainID(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:])
}

// signedPart prefixes the token's version, key and time with the signing
// context, so a signature made for another purpose never passes as a token.
func signedPart(unsigned []byte) []byte {
	return append([]byte(signContext), unsigned[1:]...)
}

type contextKey struct{}

// Middleware rejects requests without a valid "Authorization: Bearer" token
// with 401 and stores the chain ID in the request context.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		chainID, err := Verify(token, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, chainID)))
	})
}

// ChainFromContext returns the chain ID that Middleware stored.
func ChainFromContext(ctx context.Context) string {
	chainID, _ := ctx.Value(contextKey{}).(string)
	return chainID
}
