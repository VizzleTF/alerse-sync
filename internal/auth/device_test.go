package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

const testAccount = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func TestVerifyDevice(t *testing.T) {
	key := newKey(t)
	now := time.Now()
	tests := []struct {
		name   string
		issued time.Time
		err    error
	}{
		{"fresh", now, nil},
		{"almost expired", now.Add(-MaxAge + time.Minute), nil},
		{"expired", now.Add(-MaxAge - time.Minute), ErrExpired},
		{"issued in the future", now.Add(MaxSkew + time.Minute), ErrExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := VerifyDevice(NewDeviceToken(key, testAccount, test.issued), now)
			if !errors.Is(err, test.err) {
				t.Fatalf("VerifyDevice: error %v, want %v", err, test.err)
			}
			if err == nil && (got.Account != testAccount || !got.PublicKey.Equal(key.Public())) {
				t.Fatalf("VerifyDevice: %+v", got)
			}
		})
	}
}

func TestVerifyDeviceRejectsTampering(t *testing.T) {
	key := newKey(t)
	raw, _ := base64.RawURLEncoding.DecodeString(NewDeviceToken(key, testAccount, time.Now()))
	other := newKey(t)

	tests := []struct {
		name   string
		change func([]byte) []byte
		err    error
	}{
		{"other account", func(b []byte) []byte { b[1] ^= 1; return b }, ErrSignature},
		{"other device key", func(b []byte) []byte {
			copy(b[1+AccountSize:], other.Public().(ed25519.PublicKey))
			return b
		}, ErrSignature},
		{"other time", func(b []byte) []byte {
			at := 1 + AccountSize + ed25519.PublicKeySize
			binary.BigEndian.PutUint64(b[at:], binary.BigEndian.Uint64(b[at:])-1)
			return b
		}, ErrSignature},
		{"flipped signature", func(b []byte) []byte { b[len(b)-1] ^= 1; return b }, ErrSignature},
		{"chain token version", func(b []byte) []byte { b[0] = tokenVersion; return b }, ErrMalformed},
		{"truncated", func(b []byte) []byte { return b[:len(b)-1] }, ErrMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tampered := test.change(append([]byte(nil), raw...))
			_, err := VerifyDevice(base64.RawURLEncoding.EncodeToString(tampered), time.Now())
			if !errors.Is(err, test.err) {
				t.Fatalf("error %v, want %v", err, test.err)
			}
		})
	}
}

// A chain token is no device token and the other way round, and a signature
// made under the chain token's context does not pass as a device token's.
func TestTokensDoNotCross(t *testing.T) {
	key := newKey(t)
	now := time.Now()
	if _, err := VerifyDevice(NewToken(key, now), now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("chain token as device token: %v", err)
	}
	if _, err := Verify(NewDeviceToken(key, testAccount, now), now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("device token as chain token: %v", err)
	}

	raw, _ := base64.RawURLEncoding.DecodeString(NewDeviceToken(key, testAccount, now))
	unsigned := raw[:deviceTokenSize-ed25519.SignatureSize]
	signed := append([]byte(signContext), unsigned[1:]...)
	copy(raw[deviceTokenSize-ed25519.SignatureSize:], ed25519.Sign(key, signed))
	if _, err := VerifyDevice(base64.RawURLEncoding.EncodeToString(raw), now); !errors.Is(err, ErrSignature) {
		t.Fatalf("signature under the chain context: %v", err)
	}
}

func TestNewDeviceTokenNeedsAccountID(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic for a short account ID")
		}
	}()
	NewDeviceToken(newKey(t), strings.Repeat("0", 62), time.Now())
}
