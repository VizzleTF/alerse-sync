package account

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
)

// countingStore counts device lookups.
type countingStore struct {
	testStore
	lookups atomic.Int32
}

func (s *countingStore) UseDevice(ctx context.Context, accountID string, publicKey []byte, interval time.Duration) (bool, error) {
	s.lookups.Add(1)
	return s.testStore.UseDevice(ctx, accountID, publicKey, interval)
}

// TestKnownDevices checks that tokens of keys the account does not have, and
// of active devices, are answered from memory after the first lookup, and
// that a removal is not.
func TestKnownDevices(t *testing.T) {
	counting := &countingStore{}
	old := newStore
	newStore = func(t *testing.T) testStore { counting.testStore = old(t); return counting }
	t.Cleanup(func() { newStore = old })
	e := newEnv(t, Config{})
	laptop, phone := newDevice(t), newDevice(t)
	account := e.create(id(1), laptop)
	if rec := e.unlock(id(1), 3, phone, "Phone"); rec.Code != 200 {
		t.Fatalf("unlock: HTTP %d", rec.Code)
	}

	stranger := token(newDevice(t), account)
	before := counting.lookups.Load()
	for range 5 {
		if rec := e.do("GET", "/v3/devices", stranger, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unknown key: HTTP %d", rec.Code)
		}
		if rec := e.do("GET", "/v3/devices", token(laptop, account), nil); rec.Code != 200 {
			t.Fatalf("laptop: HTTP %d", rec.Code)
		}
	}
	if n := counting.lookups.Load() - before; n != 2 {
		t.Fatalf("%d device lookups for ten requests of two keys, want 2", n)
	}

	// The phone signs itself out, without the password, and is refused at
	// once on this instance.
	phoneID := hex.EncodeToString(phone.Public().(ed25519.PublicKey))
	if rec := e.do("GET", "/v3/devices", token(phone, account), nil); rec.Code != 200 {
		t.Fatalf("phone: HTTP %d", rec.Code)
	}
	if rec := e.do("DELETE", "/v3/devices/"+phoneID, token(phone, account), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("sign out: HTTP %d: %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v3/devices", token(phone, account), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("signed out device: HTTP %d", rec.Code)
	}
	// Removing another device still takes the password.
	laptopID := hex.EncodeToString(laptop.Public().(ed25519.PublicKey))
	tablet := newDevice(t)
	if rec := e.unlock(id(1), 3, tablet, "Tablet"); rec.Code != 200 {
		t.Fatalf("unlock tablet: HTTP %d", rec.Code)
	}
	if rec := e.do("DELETE", "/v3/devices/"+laptopID, token(tablet, account), nil); rec.Code == http.StatusNoContent {
		t.Fatal("a device removed another without the password")
	}
}

func TestDeviceLimits(t *testing.T) {
	e := newEnv(t, Config{
		NewDevices:     ratelimit.NewPer(2, 24*time.Hour),
		DeviceRequests: ratelimit.NewPer(3, time.Minute),
	})
	laptop := newDevice(t)
	account := e.create(id(1), laptop)
	phone := newDevice(t)
	for i, key := range []ed25519.PrivateKey{phone, newDevice(t)} {
		if rec := e.unlock(id(1), 3, key, "Device"); rec.Code != 200 {
			t.Fatalf("new device %d: HTTP %d", i, rec.Code)
		}
	}
	if rec := e.unlock(id(1), 3, newDevice(t), "Third"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a third new device in a day: HTTP %d", rec.Code)
	}
	// Signing in again with a key the account has is not a new device.
	if rec := e.unlock(id(1), 3, phone, "Phone"); rec.Code != 200 {
		t.Fatalf("the same key again: HTTP %d", rec.Code)
	}

	for i := range 3 {
		if rec := e.do("GET", "/v3/devices", token(laptop, account), nil); rec.Code != 200 {
			t.Fatalf("request %d: HTTP %d", i, rec.Code)
		}
	}
	if rec := e.do("GET", "/v3/devices", token(laptop, account), nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the device's limit: HTTP %d", rec.Code)
	}
}

// TestUnknownLookups checks that tokens of accounts the instance has not
// seen share one small budget of database lookups, and real devices keep
// working past it.
func TestUnknownLookups(t *testing.T) {
	e := newEnv(t, Config{UnknownLookups: ratelimit.NewPer(1, time.Hour)})
	laptop := newDevice(t)
	account := e.create(id(1), laptop)
	made := func() string { return token(newDevice(t), id(byte(9))) }
	if rec := e.do("GET", "/v3/devices", made(), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("first made-up token: HTTP %d", rec.Code)
	}
	if rec := e.do("GET", "/v3/devices", made(), nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("second made-up token: HTTP %d, want 503 without a lookup", rec.Code)
	}
	if rec := e.do("GET", "/v3/devices", token(laptop, account), nil); rec.Code != 200 {
		t.Fatalf("a device of a known account: HTTP %d", rec.Code)
	}
}
