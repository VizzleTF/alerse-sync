package auth

import (
	"crypto/ed25519"
	"testing"
	"time"
)

func FuzzVerify(f *testing.F) {
	_, key, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	f.Add(NewToken(key, now))
	f.Add("")
	f.Add("AQ")
	f.Fuzz(func(t *testing.T, token string) {
		chainID, err := Verify(token, now)
		if err == nil && len(chainID) != 64 {
			t.Fatalf("chain ID %q", chainID)
		}
	})
}
