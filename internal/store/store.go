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
)

// ErrNoChain is returned by Update when the chain does not exist and create
// is false.
var ErrNoChain = errors.New("sync chain does not exist")

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

// Change is an entity version to fetch: ChangesSince returns entities with a
// version above After.
type Change struct {
	DataType int32
	After    int64
}

// Tx is a write transaction on one chain.
type Tx interface {
	// Chain returns the chain as of the start of the transaction, with the
	// version and live entity count updated by Put.
	Chain() Chain
	// Created reports whether this transaction created the chain.
	Created() bool
	// Get returns the entity with id, or nil.
	Get(ctx context.Context, id string) (*Entity, error)
	// Put inserts or replaces an entity. Its version must come from
	// NextVersion.
	Put(ctx context.Context, entity Entity) error
	// NextVersion hands out the chain's next version.
	NextVersion() int64
}

// Store is the storage of all sync chains.
type Store interface {
	// Chain returns a chain, or false if it does not exist.
	Chain(ctx context.Context, chainID string) (Chain, bool, error)
	// Update runs fn in a transaction on the chain, creating the chain first
	// if create is set. The transaction commits when fn returns nil.
	Update(ctx context.Context, chainID string, create bool, fn func(Tx) error) error
	// ChangesSince returns up to limit entities of a data type with a version
	// above after, in version order.
	ChangesSince(ctx context.Context, chainID string, change Change, limit int) ([]Entity, error)
	// CountSince counts the entities ChangesSince would return without a
	// limit.
	CountSince(ctx context.Context, chainID string, change Change) (int64, error)
	// Delete removes a chain and its entities if it still has birthday.
	Delete(ctx context.Context, chainID, birthday string) error
	// CountChains counts the chains.
	CountChains(ctx context.Context) (int64, error)
}

// NewChainSecrets returns a birthday and a keystore key for a new chain.
func NewChainSecrets() (birthday string, keystoreKey []byte) {
	keystoreKey = make([]byte, 16)
	_, _ = rand.Read(keystoreKey) // never fails
	return rand.Text(), keystoreKey
}
