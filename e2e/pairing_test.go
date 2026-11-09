//go:build integration

package e2e

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
)

// TestPairingOnPostgres relays a pairing through the server and checks that
// clearing the chain removes its slots.
func TestPairingOnPostgres(t *testing.T) {
	key := newChain(t)
	a := &device{t: t, key: key, guid: "pair-a"}
	a.bookmarks() // creates the chain
	token := auth.NewToken(key, time.Now())
	slot := testLoginID("pairing slot")
	msg := bytes.Repeat([]byte{1}, 32)

	if status, body := call(t, "POST", "/pair", token, map[string]any{"slot": slot, "msg": msg}); status != 204 {
		t.Fatalf("open: HTTP %d: %s", status, body)
	}
	if status, _ := call(t, "POST", "/pair", auth.NewToken(newChain(t), time.Now()), map[string]any{"slot": slot, "msg": msg}); status != 409 {
		t.Fatalf("open a taken slot: HTTP %d", status)
	}
	join := map[string]any{"msg": bytes.Repeat([]byte{2}, 32), "name": "Laptop", "confirm": bytes.Repeat([]byte{3}, 32)}
	if status, body := call(t, "POST", "/pair/"+slot+"/join", "", join); status != 204 {
		t.Fatalf("join: HTTP %d: %s", status, body)
	}
	if status, _ := call(t, "POST", "/pair/"+slot+"/join", "", join); status != 409 {
		t.Fatalf("second join: HTTP %d", status)
	}
	box := bytes.Repeat([]byte{9}, 61)
	if status, body := call(t, "POST", "/pair/"+slot+"/approve", token, map[string]any{"box": box}); status != 204 {
		t.Fatalf("approve: HTTP %d: %s", status, body)
	}
	status, body := call(t, "GET", "/pair/"+slot, "", nil)
	var got struct {
		Msg, BMsg, Box []byte
		Name           string
		Joined         bool
		Expires        int
	}
	if status != 200 || json.Unmarshal(body, &got) != nil || !bytes.Equal(got.Msg, msg) || !got.Joined ||
		got.Name != "Laptop" || !bytes.Equal(got.Box, box) || got.Expires < 250 {
		t.Fatalf("get: HTTP %d: %s", status, body)
	}

	// The slot goes with its chain.
	if _, err := pool.Exec(t.Context(), `DELETE FROM chains WHERE id = $1`, auth.ChainID(key.Public().(ed25519.PublicKey))); err != nil {
		t.Fatal(err)
	}
	if status, _ := call(t, "GET", "/pair/"+slot, "", nil); status != 404 {
		t.Fatalf("slot after the chain went: HTTP %d", status)
	}
}
