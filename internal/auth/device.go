package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// A device token is what a device of an account (/v3) sends with every
// request. Each device has an Ed25519 key of its own, so the server can tell
// devices apart and refuse a removed one:
//
//	token     = base64url(0x02 | account (32) | device public key (32) | issued at (8) | signature (64))
//	issued at = Unix time in milliseconds, big-endian
//	signature = Ed25519(device key, "alerse-sync-device-token-v1" | account | public key | issued at)
//
// The time window is the chain token's. A valid signature proves only that
// the device holds its key; the caller checks that the key is an active
// device of the account.
const (
	deviceTokenVersion = 0x02
	deviceTokenSize    = 1 + AccountSize + ed25519.PublicKeySize + 8 + ed25519.SignatureSize
	deviceSignContext  = "alerse-sync-device-token-v1"

	// AccountSize is the size of an account ID in bytes.
	AccountSize = 32
)

// Device is who signed a device token.
type Device struct {
	// Account is the account ID, 64 lowercase hex digits.
	Account string
	// PublicKey is the device's Ed25519 public key.
	PublicKey ed25519.PublicKey
}

// ID names the device in the API: its public key as 64 lowercase hex
// digits.
func (d Device) ID() string { return hex.EncodeToString(d.PublicKey) }

// NewDeviceToken issues a token of the device key for account (64 hex
// digits) at time now.
func NewDeviceToken(key ed25519.PrivateKey, account string, now time.Time) string {
	accountID, err := hex.DecodeString(account)
	if err != nil || len(accountID) != AccountSize {
		panic("auth: account ID must be 64 hex digits")
	}
	token := make([]byte, 0, deviceTokenSize)
	token = append(token, deviceTokenVersion)
	token = append(token, accountID...)
	token = append(token, key.Public().(ed25519.PublicKey)...)
	token = binary.BigEndian.AppendUint64(token, uint64(now.UnixMilli()))
	token = append(token, ed25519.Sign(key, deviceSignedPart(token))...)
	return base64.RawURLEncoding.EncodeToString(token)
}

// VerifyDevice checks a device token at time now and returns who signed it.
func VerifyDevice(token string, now time.Time) (Device, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != deviceTokenSize || raw[0] != deviceTokenVersion {
		return Device{}, ErrMalformed
	}
	account := raw[1 : 1+AccountSize]
	public := ed25519.PublicKey(raw[1+AccountSize : 1+AccountSize+ed25519.PublicKeySize])
	issuedAt := time.UnixMilli(int64(binary.BigEndian.Uint64(raw[1+AccountSize+ed25519.PublicKeySize:])))
	signature := raw[deviceTokenSize-ed25519.SignatureSize:]
	if issuedAt.Before(now.Add(-MaxAge)) || issuedAt.After(now.Add(MaxSkew)) {
		return Device{}, ErrExpired
	}
	if !ed25519.Verify(public, deviceSignedPart(raw[:deviceTokenSize-ed25519.SignatureSize]), signature) {
		return Device{}, ErrSignature
	}
	return Device{Account: hex.EncodeToString(account), PublicKey: append(ed25519.PublicKey(nil), public...)}, nil
}

func deviceSignedPart(unsigned []byte) []byte {
	return append([]byte(deviceSignContext), unsigned[1:]...)
}

type deviceKey struct{}

// WithDevice returns ctx with the device that signed the request.
func WithDevice(ctx context.Context, d Device) context.Context {
	return context.WithValue(ctx, deviceKey{}, d)
}

// DeviceFromContext returns the device WithDevice stored, or false.
func DeviceFromContext(ctx context.Context) (Device, bool) {
	d, ok := ctx.Value(deviceKey{}).(Device)
	return d, ok
}

// WithChain returns ctx with the chain a request is about, as Middleware
// stores it, for handlers that check access to the chain in another way.
func WithChain(ctx context.Context, chainID string) context.Context {
	return context.WithValue(ctx, contextKey{}, chainID)
}
