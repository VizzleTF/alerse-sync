// Package store keeps sync chains and their entities.
//
// Every write to a chain runs in an Update transaction that holds the chain's
// lock, so the chain's version counter only grows and a reader never sees a
// version before the versions below it. Readers do not take the lock.
package store

import (
	"context"
	"crypto/rand"
	"errors"
	"time"
)

var (
	// ErrNoChain is returned by Update when the chain does not exist.
	ErrNoChain = errors.New("sync chain does not exist")
	// ErrFull is returned by Create when the server already keeps the
	// maximum number of chains.
	ErrFull = errors.New("the server has no room for another sync chain")
)

// TypeState is what a chain holds of one data type.
type TypeState struct {
	// Version is the highest version an entity of the type has had.
	Version int64
	// PurgedVersion is the highest version of a tombstone of the type that
	// was removed. A client that has not seen it may have missed a deletion.
	PurgedVersion int64
	// Entities counts the live entities, Tombstones the deleted ones.
	Entities   int64
	Tombstones int64
	// Bytes is the size of the stored entities, tombstones included.
	Bytes int64
}

// Chain is a sync chain's state.
type Chain struct {
	ID string
	// Birthday changes when the chain's data is cleared; a client with an
	// older birthday has to start over.
	Birthday string
	// KeystoreKey lets clients encrypt data without a passphrase.
	KeystoreKey []byte
	// Version is the highest entity version handed out.
	Version int64
	// LiveEntities counts entities that are not deleted.
	LiveEntities int64
	// Bytes is the size of all stored entities, tombstones included.
	Bytes int64
	// CustomPassphrase is set once the chain's Nigori node says the data is
	// encrypted with a passphrase the server does not know.
	CustomPassphrase bool
	// LastSeen is when a device last talked to the server about the chain,
	// to the hour.
	LastSeen time.Time
	// Types holds the state of each data type the chain has entities of.
	Types map[int32]TypeState
}

// Entity is one stored sync entity.
type Entity struct {
	ID       string
	DataType int32
	Version  int64
	Deleted  bool
	// ServerTag is set for entities the server created, such as the
	// bookmark bar folder.
	ServerTag string
	// Data is the SyncEntity message served to clients.
	Data []byte
}

// Change selects entity versions to fetch: ChangesSince returns entities of
// DataType with a version above After and, if Until is set, at most Until.
// LiveOnly leaves out tombstones.
type Change struct {
	DataType int32
	After    int64
	Until    int64
	LiveOnly bool
}

// Tx is a write transaction on one chain.
type Tx interface {
	// Chain returns the chain as of the start of the transaction, with its
	// version, counts and sizes updated by the writes so far.
	Chain() Chain
	// Created reports whether this transaction created the chain.
	Created() bool
	// Prefetch reads the entities with ids, so the Gets that follow need no
	// round trip to the database each.
	Prefetch(ctx context.Context, ids []string) error
	// Get returns the entity with id, or nil.
	Get(ctx context.Context, id string) (*Entity, error)
	// Put inserts or replaces an entity. Its version must come from
	// NextVersion. Writes may be buffered until the transaction ends.
	Put(ctx context.Context, entity Entity) error
	// NextVersion hands out the chain's next version.
	NextVersion() int64
	// Evict removes the oldest entities of a data type, by version, until it
	// has removed at least entities entities and bytes bytes or there are no
	// more. It leaves no tombstones, and skips entities this transaction read
	// or wrote. It returns how many entities and bytes it removed.
	Evict(ctx context.Context, dataType int32, entities, bytes int64) (int64, int64, error)
	// SetCustomPassphrase records whether the chain's data is encrypted with
	// a custom passphrase.
	SetCustomPassphrase(bool)
}

// Retention says what Collect removes.
type Retention struct {
	// Now is the time the ages below count from.
	Now time.Time
	// TombstoneTTL is how long tombstones are kept; 0 keeps them forever.
	// Removing them raises their data type's PurgedVersion.
	TombstoneTTL time.Duration
	// ExpiringTypes are data types whose entities expire after
	// ExpiringTypesTTL since they were last written, like browsing history.
	// They are removed without raising PurgedVersion: clients expire them
	// on their own.
	ExpiringTypes    []int32
	ExpiringTypesTTL time.Duration
	// EmptyChainTTL removes chains that no device committed to, at most
	// EmptyVersion, after that long; 0 keeps them.
	EmptyChainTTL time.Duration
	EmptyVersion  int64
	// InactiveChainTTL removes chains no device talked to for that long; 0
	// keeps them.
	InactiveChainTTL time.Duration
	// Batch bounds the rows one statement removes.
	Batch int
}

// Collected counts what Collect removed.
type Collected struct {
	Tombstones     int64
	Expired        int64
	EmptyChains    int64
	InactiveChains int64
}

// Store is the storage of all sync chains.
type Store interface {
	// Chain returns a chain, or false if it does not exist.
	Chain(ctx context.Context, chainID string) (Chain, bool, error)
	// Update runs fn in a transaction on an existing chain; it returns
	// ErrNoChain if there is none. The transaction commits when fn returns
	// nil.
	Update(ctx context.Context, chainID string, fn func(Tx) error) error
	// Create runs fn in a transaction on the chain, creating the chain first
	// if it does not exist. It returns ErrFull when it would create a chain
	// while the server keeps maxChains chains; 0 means no limit.
	Create(ctx context.Context, chainID string, maxChains int64, fn func(Tx) error) error
	// ChangesSince returns up to limit entities that match change, in
	// version order.
	ChangesSince(ctx context.Context, chainID string, change Change, limit int) ([]Entity, error)
	// CountSince counts the entities ChangesSince would return without a
	// limit, counting at most limit.
	CountSince(ctx context.Context, chainID string, change Change, limit int) (int64, error)
	// Touch records that a device talked to the server about the chain.
	Touch(ctx context.Context, chainID string) error
	// Delete removes a chain and its entities if it still has birthday.
	Delete(ctx context.Context, chainID, birthday string) error
	// CountChains counts the chains.
	CountChains(ctx context.Context) (int64, error)
	// Collect removes what r says is due, a batch at a time, until nothing
	// is left or ctx ends.
	Collect(ctx context.Context, r Retention) (Collected, error)
}

// NewChainSecrets returns a birthday and a keystore key for a new chain.
func NewChainSecrets() (birthday string, keystoreKey []byte) {
	keystoreKey = make([]byte, 16)
	_, _ = rand.Read(keystoreKey) // never fails
	return rand.Text(), keystoreKey
}

// TouchInterval is how often LastSeen is updated.
const TouchInterval = time.Hour

// Clone returns a copy of c that shares no map with it.
func (c Chain) Clone() Chain {
	types := make(map[int32]TypeState, len(c.Types))
	for dataType, state := range c.Types {
		types[dataType] = state
	}
	c.Types = types
	return c
}

// Account updates c's counts and sizes for an entity that changes from old to
// new. old is nil for a new entity, new is nil for a removed one.
func (c *Chain) Account(old, new *Entity) {
	if old != nil {
		c.Remove(old.DataType, old.Deleted, int64(len(old.Data)))
	}
	if new == nil {
		return
	}
	if c.Types == nil {
		c.Types = map[int32]TypeState{}
	}
	state := c.Types[new.DataType]
	state.Bytes += int64(len(new.Data))
	c.Bytes += int64(len(new.Data))
	if new.Deleted {
		state.Tombstones++
	} else {
		state.Entities++
		c.LiveEntities++
	}
	state.Version = max(state.Version, new.Version)
	c.Types[new.DataType] = state
}

// Remove updates c's counts and sizes for a removed entity of size bytes.
func (c *Chain) Remove(dataType int32, deleted bool, size int64) {
	if c.Types == nil {
		c.Types = map[int32]TypeState{}
	}
	state := c.Types[dataType]
	state.Bytes -= size
	c.Bytes -= size
	if deleted {
		state.Tombstones--
	} else {
		state.Entities--
		c.LiveEntities--
	}
	c.Types[dataType] = state
}
