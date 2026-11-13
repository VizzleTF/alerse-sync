package auth

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func FuzzVerifyDevice(f *testing.F) {
	_, key, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	f.Add(NewDeviceToken(key, strings.Repeat("ab", 32), now))
	f.Add("")
	f.Add("Ag")
	f.Fuzz(func(t *testing.T, token string) {
		d, err := VerifyDevice(token, now)
		if err == nil && (len(d.Account) != 64 || len(d.PublicKey) != ed25519.PublicKeySize) {
			t.Fatalf("device %+v", d)
		}
	})
}
