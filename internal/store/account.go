package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrEnvelopeTaken is returned by CreateAccount when another account
	// has an envelope with the same ID.
	ErrEnvelopeTaken = errors.New("the envelope ID belongs to another account")
	// ErrNoAccount is returned when the account does not exist.
	ErrNoAccount = errors.New("account does not exist")
	// ErrNoDevice is returned when the account has no such active device.
	ErrNoDevice = errors.New("no such device")
	// ErrDeviceRemoved is returned by AddDevice for a key that was removed
	// from the account: a removed key never comes back.
	ErrDeviceRemoved = errors.New("the device key was removed from the account")
	// ErrTooManyDevices is returned by AddDevice when the account has the
	// maximum number of active devices.
	ErrTooManyDevices = errors.New("the account has too many devices")
	// ErrLastDevice is returned by RemoveDevice for the account's only
	// active device.
	ErrLastDevice = errors.New("the account's last device cannot be removed")
	// ErrChainTaken is returned by ClaimChain when the chain belongs to
	// another account or is a chain of the old scheme, which no account may
	// take over.
	ErrChainTaken = errors.New("the sync chain is not the account's")
	// ErrTooManyChains is returned by ClaimChain when the account has the
	// maximum number of chains.
	ErrTooManyChains = errors.New("the account has too many sync chains")
)

// AccountKindSK is the kind of an account opened with a secret key (24
// words) and a password.
const AccountKindSK = "sk"

// Account is a user's account (/v3): a set of devices and the chains they
// sync. The server holds the account key only inside envelopes it cannot
// open; see docs/protocol.md.
type Account struct {
	// ID is random, 64 lowercase hex digits. Device tokens name it.
	ID        string
	Kind      string
	CreatedAt time.Time
}

// Envelope is the account key, wrapped by the client with a key the server
// never sees, and what the client needs to open it again.
type Envelope struct {
	// ID is the lookup key the client derives, 64 lowercase hex digits.
	ID        string
	AccountID string
	Scheme    string
	// Salt and KDF (the parameters, as JSON) were chosen by the client.
	Salt []byte
	KDF  []byte
	// Verifier is Argon2id (with VerifierSalt) of the client's
	// authentication key, never the key itself.
	Verifier     []byte
	VerifierSalt []byte
	// Wrapped is the account key, encrypted by the client.
	Wrapped   []byte
	UpdatedAt time.Time
}

// Device is a device's key in an account.
type Device struct {
	AccountID string
	PublicKey []byte
	Name      string
	CreatedAt time.Time
	LastSeen  time.Time
}

// AccountStore keeps accounts, their envelopes, devices and chains.
type AccountStore interface {
	// CreateAccount stores a new account with its first envelope and
	// device. It returns ErrEnvelopeTaken if the envelope ID is in use.
	CreateAccount(ctx context.Context, a Account, e Envelope, d Device) error
	// Account returns the account with id, or false.
	Account(ctx context.Context, id string) (Account, bool, error)
	// Envelope returns the envelope with id, or false.
	Envelope(ctx context.Context, id string) (Envelope, bool, error)
	// AccountEnvelope returns the account's envelope of scheme, or false.
	AccountEnvelope(ctx context.Context, accountID, scheme string) (Envelope, bool, error)
	// AddDevice adds d to its account, or renames it if it is an active
	// device of the account already. It returns ErrDeviceRemoved for a key
	// removed from the account, ErrTooManyDevices when the account has
	// maxDevices active devices (0: no limit), and ErrNoAccount.
	AddDevice(ctx context.Context, d Device, maxDevices int) error
	// UseDevice reports whether publicKey is an active device of the
	// account, and records that it was seen if it was not for interval.
	UseDevice(ctx context.Context, accountID string, publicKey []byte, interval time.Duration) (bool, error)
	// Devices returns the account's active devices, oldest first.
	Devices(ctx context.Context, accountID string) ([]Device, error)
	// RemoveDevice removes an active device of the account. It returns
	// ErrNoDevice if there is none with publicKey, and ErrLastDevice if it
	// is the only one.
	RemoveDevice(ctx context.Context, accountID string, publicKey []byte) error
	// ChainAccount returns the account that has claimed chainID, or false.
	ChainAccount(ctx context.Context, chainID string) (string, bool, error)
	// ClaimChain makes chainID one of the account's chains if it is not
	// already. Only a chain that does not exist yet can be claimed: it
	// returns ErrChainTaken for a chain of another account or an existing
	// chain of no account, ErrTooManyChains when the account has maxChains
	// chains (0: no limit), and ErrNoAccount. A claim outlives its chain,
	// so a chain cleared by ClearServerData stays the account's.
	ClaimChain(ctx context.Context, accountID, chainID string, maxChains int) error
	// DeleteAccount removes the account, its envelopes, devices and claims,
	// and the chains it claimed with all their data. It returns the chains
	// that were removed.
	DeleteAccount(ctx context.Context, accountID string) ([]string, error)
	// CountAccounts counts the accounts.
	CountAccounts(ctx context.Context) (int64, error)
	// ServerSecret returns a random secret of the server, the same for all
	// its instances and restarts, creating it on first use.
	ServerSecret(ctx context.Context, name string) ([]byte, error)
}
