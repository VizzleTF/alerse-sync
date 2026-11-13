// Package credential has what the password-based sign-ins share: the client's
// KDF parameters and their bounds, the server's Argon2id hash of
// authentication keys, and made-up values that keep unknown ids from being
// told apart from known ones.
package credential

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"sync"

	"golang.org/x/crypto/argon2"
)

// KDF is the client's key derivation, with its parameters.
type KDF struct {
	Name string `json:"name"`
	N    int64  `json:"n"`
	R    int64  `json:"r"`
	P    int64  `json:"p"`
}

// DefaultKDF is what the server answers for ids it does not know, and what
// clients use today: scrypt with 128 MiB.
var DefaultKDF = KDF{Name: "scrypt", N: 1 << 17, R: 8, P: 1}

// Valid accepts scrypt with at least 32 MiB and at most 1 GiB of memory.
func (k KDF) Valid() bool {
	return k.Name == "scrypt" && k.N >= 1<<15 && k.N <= 1<<20 && k.N&(k.N-1) == 0 &&
		k.R >= 1 && k.R <= 32 && k.P >= 1 && k.P <= 16 && 128*k.R*k.N <= 1<<30
}

// Hash is the Argon2id cost of the server's hash of authentication keys.
type Hash struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
}

// DefaultHash is OWASP's minimum for Argon2id: 19 MiB, 2 passes.
var DefaultHash = Hash{Time: 2, Memory: 19 * 1024, Threads: 1}

// hashSlots bounds the Argon2id hashes computed at once, which bounds their
// memory.
const hashSlots = 4

// Hasher computes Argon2id hashes, a few at a time. Share one between the
// handlers that hash, so their memory adds up to one bound.
type Hasher struct {
	cost  Hash
	slots chan struct{}
}

// NewHasher returns a hasher of cost, or of DefaultHash if cost is zero.
func NewHasher(cost Hash) *Hasher {
	if cost == (Hash{}) {
		cost = DefaultHash
	}
	return &Hasher{cost: cost, slots: make(chan struct{}, hashSlots)}
}

// Sum is the stored form of an authentication key: Argon2id, so that the
// database alone does not give a cheap way to test passwords. It fails only
// when ctx ends while it waits for a slot.
func (h *Hasher) Sum(ctx context.Context, key, salt []byte) ([]byte, error) {
	select {
	case h.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-h.slots }()
	return argon2.IDKey(key, salt, h.cost.Time, h.cost.Memory, h.cost.Threads, 32), nil
}

// Fakes makes up values for ids the server does not know, the same on every
// request and instance, from a random server secret.
type Fakes struct {
	// Secret reads the server secret; it is called until it succeeds once.
	Secret func(ctx context.Context) ([]byte, error)

	mu  sync.Mutex
	key []byte
}

// Value returns the made-up value of kind for id.
func (f *Fakes) Value(ctx context.Context, kind, id string) ([]byte, error) {
	key, err := f.secret(ctx)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(kind + "\x00" + id))
	return mac.Sum(nil), nil
}

// secret reads the server secret once: it never changes, and reading it on
// every request would make an unknown id answer slower than a known one.
func (f *Fakes) secret(ctx context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.key == nil {
		key, err := f.Secret(ctx)
		if err != nil {
			return nil, err
		}
		f.key = key
	}
	return f.key, nil
}
