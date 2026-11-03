package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestVerify(t *testing.T) {
	key := newKey(t)
	now := time.Now()
	want := ChainID(key.Public().(ed25519.PublicKey))

	tests := []struct {
		name   string
		issued time.Time
		err    error
	}{
		{"fresh", now, nil},
		{"almost expired", now.Add(-MaxAge + time.Minute), nil},
		{"expired", now.Add(-MaxAge - time.Minute), ErrExpired},
		{"small clock skew", now.Add(MaxSkew - time.Second), nil},
		{"issued in the future", now.Add(MaxSkew + time.Minute), ErrExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Verify(NewToken(key, test.issued), now)
			if !errors.Is(err, test.err) {
				t.Fatalf("Verify: error %v, want %v", err, test.err)
			}
			if err == nil && got != want {
				t.Fatalf("Verify: chain %q, want %q", got, want)
			}
		})
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	key := newKey(t)
	raw, _ := base64.RawURLEncoding.DecodeString(NewToken(key, time.Now()))
	other := newKey(t).Public().(ed25519.PublicKey)

	tests := []struct {
		name   string
		change func([]byte) []byte
		err    error
	}{
		{"other key", func(b []byte) []byte { copy(b[1:], other); return b }, ErrSignature},
		{"other time", func(b []byte) []byte { b[40]++; return b }, ErrSignature},
		{"other signature", func(b []byte) []byte { b[len(b)-1]++; return b }, ErrSignature},
		{"unknown version", func(b []byte) []byte { b[0] = 2; return b }, ErrMalformed},
		{"truncated", func(b []byte) []byte { return b[:len(b)-1] }, ErrMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token := test.change(append([]byte(nil), raw...))
			_, err := Verify(base64.RawURLEncoding.EncodeToString(token), time.Now())
			if !errors.Is(err, test.err) {
				t.Fatalf("Verify: error %v, want %v", err, test.err)
			}
		})
	}
	if _, err := Verify("not base64!", time.Now()); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify: error %v, want %v", err, ErrMalformed)
	}
}

func TestMiddleware(t *testing.T) {
	key := newKey(t)
	var seen string
	handler := Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = ChainFromContext(r.Context())
	}))

	tests := []struct {
		name   string
		header string
		status int
	}{
		{"valid", "Bearer " + NewToken(key, time.Now()), http.StatusOK},
		{"no header", "", http.StatusUnauthorized},
		{"other scheme", "Basic " + NewToken(key, time.Now()), http.StatusUnauthorized},
		{"bad token", "Bearer abc", http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			seen = ""
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if test.header != "" {
				req.Header.Set("Authorization", test.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("status %d, want %d", rec.Code, test.status)
			}
			if wantSeen := test.status == http.StatusOK; (seen != "") != wantSeen {
				t.Fatalf("handler saw chain %q", seen)
			}
		})
	}
}
