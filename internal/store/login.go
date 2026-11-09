package store

import (
	"context"
	"errors"
	"time"
)

// ErrLoginTaken is returned by SetLogin when the login ID belongs to another
// chain.
var ErrLoginTaken = errors.New("the login belongs to another sync chain")

// Login is a chain's optional sign-in: what lets a new device fetch the
// chain's secret, wrapped with a key derived from a login name and password,
// without the 24 words. The server knows neither the login name nor the
// password; see docs/protocol.md.
type Login struct {
	// ID is hex(SHA-256 of the normalized login name, with a context): the
	// lookup key. At most one login per ID, and one per chain.
	ID      string
	ChainID string
	// Scheme names how the client derives its keys and wraps the secret,
	// so a later scheme (such as OPAQUE) can sit next to this one.
	Scheme string
	// Salt and KDF are what the client needs to derive its keys again; the
	// client chose them. KDF is the scheme's parameters, as JSON.
	Salt []byte
	KDF  []byte
	// Verifier is a slow hash (Argon2id, with VerifierSalt) of the client's
	// authentication key, never the key itself.
	Verifier     []byte
	VerifierSalt []byte
	// Secret is the chain's secret, encrypted by the client with a key the
	// server never sees.
	Secret    []byte
	UpdatedAt time.Time
}

// LoginStore keeps chains' sign-ins.
type LoginStore interface {
	// Login returns the login with id, or false.
	Login(ctx context.Context, id string) (Login, bool, error)
	// ChainLogin returns the login of chainID, or false.
	ChainLogin(ctx context.Context, chainID string) (Login, bool, error)
	// SetLogin stores l as its chain's only login, replacing the chain's
	// earlier one. It returns ErrLoginTaken if another chain has l.ID, and
	// ErrNoChain if the chain does not exist. A chain's login goes when the
	// chain goes.
	SetLogin(ctx context.Context, l Login) error
	// DeleteLogin removes the login of chainID, if any.
	DeleteLogin(ctx context.Context, chainID string) error
	// ServerSecret returns a random secret of the server, the same for all
	// its instances and restarts, creating it on first use.
	ServerSecret(ctx context.Context, name string) ([]byte, error)
}
