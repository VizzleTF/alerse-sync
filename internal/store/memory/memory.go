// Package memory is an in-memory store.Store for tests.
package memory

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

type chain struct {
	state    store.Chain
	entities map[string]store.Entity
}

// Store keeps chains in memory. Its zero value is not usable; call New.
type Store struct {
	mu     sync.Mutex
	chains map[string]*chain
}

// New returns an empty store.
func New() *Store {
	return &Store{chains: map[string]*chain{}}
}

func (s *Store) Chain(_ context.Context, chainID string) (store.Chain, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chains[chainID]
	if !ok {
		return store.Chain{}, false, nil
	}
	return c.state, true, nil
}

func (s *Store) Update(_ context.Context, chainID string, create bool, fn func(store.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chains[chainID]
	created := false
	if !ok {
		if !create {
			return store.ErrNoChain
		}
		birthday, key := store.NewChainSecrets()
		c = &chain{
			state:    store.Chain{ID: chainID, Birthday: birthday, KeystoreKey: key},
			entities: map[string]store.Entity{},
		}
		created = true
	}
	t := &tx{chain: c, state: c.state, created: created, writes: map[string]store.Entity{}}
	if err := fn(t); err != nil {
		return err
	}
	c.state = t.state
	for id, e := range t.writes {
		c.entities[id] = e
	}
	s.chains[chainID] = c
	return nil
}

func (s *Store) ChangesSince(_ context.Context, chainID string, change store.Change, limit int) ([]store.Entity, error) {
	changes := s.since(chainID, change)
	return changes[:min(limit, len(changes))], nil
}

func (s *Store) CountSince(_ context.Context, chainID string, change store.Change) (int64, error) {
	return int64(len(s.since(chainID, change))), nil
}

func (s *Store) since(chainID string, change store.Change) []store.Entity {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chains[chainID]
	if !ok {
		return nil
	}
	var changes []store.Entity
	for _, e := range c.entities {
		if e.DataType == change.DataType && e.Version > change.After {
			changes = append(changes, e)
		}
	}
	slices.SortFunc(changes, func(a, b store.Entity) int { return cmp.Compare(a.Version, b.Version) })
	return changes
}

func (s *Store) Delete(_ context.Context, chainID, birthday string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.chains[chainID]; ok && c.state.Birthday == birthday {
		delete(s.chains, chainID)
	}
	return nil
}

func (s *Store) CountChains(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.chains)), nil
}

// tx buffers writes until Update applies them, so a failed fn changes
// nothing.
type tx struct {
	chain   *chain
	state   store.Chain
	created bool
	writes  map[string]store.Entity
}

func (t *tx) Chain() store.Chain { return t.state }
func (t *tx) Created() bool      { return t.created }

func (t *tx) NextVersion() int64 {
	t.state.Version++
	return t.state.Version
}

func (t *tx) Get(_ context.Context, id string) (*store.Entity, error) {
	if e, ok := t.writes[id]; ok {
		return &e, nil
	}
	if e, ok := t.chain.entities[id]; ok {
		return &e, nil
	}
	return nil, nil
}

func (t *tx) Put(ctx context.Context, e store.Entity) error {
	old, _ := t.Get(ctx, e.ID)
	wasLive := old != nil && !old.Deleted
	switch {
	case wasLive && e.Deleted:
		t.state.LiveEntities--
	case !wasLive && !e.Deleted:
		t.state.LiveEntities++
	}
	t.writes[e.ID] = e
	return nil
}
