package auth

import (
	"crypto/ed25519"
	"testing"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestChainID(t *testing.T) {
	key := newKey(t)
	id := ChainID(key.Public().(ed25519.PublicKey))
	if len(id) != 64 || id != ChainID(key.Public().(ed25519.PublicKey)) {
		t.Fatalf("chain ID %q", id)
	}
}
