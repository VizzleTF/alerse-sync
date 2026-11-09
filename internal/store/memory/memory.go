// Package memory is an in-memory store.Store for tests.
package memory

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

type entry struct {
	store.Entity
	updated time.Time
}

type chain struct {
	state    store.Chain
	created  time.Time
	entities map[string]entry
	// login is the chain's sign-in, if any; it goes with the chain.
	login *store.Login
}

// Store keeps chains in memory. Its zero value is not usable; call New.
type Store struct {
	// Now is the store's clock, which tests may replace.
	Now func() time.Time

	mu      sync.Mutex
	chains  map[string]*chain
	secrets map[string][]byte
}

// New returns an empty store.
func New() *Store {
	return &Store{Now: time.Now, chains: map[string]*chain{}}
}

func (s *Store) Chain(_ context.Context, chainID string) (store.Chain, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chains[chainID]
	if !ok {
		return store.Chain{}, false, nil
	}
	return c.state.Clone(), true, nil
}

func (s *Store) Update(_ context.Context, chainID string, fn func(store.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chains[chainID]
	if !ok {
		return store.ErrNoChain
	}
	return s.run(c, false, fn)
}

func (s *Store) Create(_ context.Context, chainID string, maxChains int64, fn func(store.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chains[chainID]
	if ok {
		return s.run(c, false, fn)
	}
	if maxChains > 0 && int64(len(s.chains)) >= maxChains {
		return store.ErrFull
	}
	birthday, key := store.NewChainSecrets()
	now := s.Now()
	c = &chain{
		state: store.Chain{
			ID: chainID, Birthday: birthday, KeystoreKey: key, LastSeen: now,
			Types: map[int32]store.TypeState{},
		},
		created:  now,
		entities: map[string]entry{},
	}
	if err := s.run(c, true, fn); err != nil {
		return err
	}
	s.chains[chainID] = c
	return nil
}

// run runs fn on c and applies its writes if it succeeds. s.mu is held.
func (s *Store) run(c *chain, created bool, fn func(store.Tx) error) error {
	t := &tx{
		chain: c, state: c.state.Clone(), created: created,
		writes: map[string]*store.Entity{}, read: map[string]bool{},
	}
	if err := fn(t); err != nil {
		return err
	}
	now := s.Now()
	if t.state.Version != c.state.Version {
		t.state.LastSeen = now
	}
	c.state = t.state
	for id, e := range t.writes {
		if e == nil {
			delete(c.entities, id)
			continue
		}
		c.entities[id] = entry{Entity: *e, updated: now}
	}
	return nil
}

func (s *Store) ChangesSince(_ context.Context, chainID string, change store.Change, limit int) ([]store.Entity, error) {
	changes := s.since(chainID, change)
	return changes[:min(limit, len(changes))], nil
}

func (s *Store) CountSince(_ context.Context, chainID string, change store.Change, limit int) (int64, error) {
	return int64(min(limit, len(s.since(chainID, change)))), nil
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
		if e.DataType == change.DataType && e.Version > change.After &&
			(change.Until == 0 || e.Version <= change.Until) && (!change.LiveOnly || !e.Deleted) {
			changes = append(changes, e.Entity)
		}
	}
	slices.SortFunc(changes, func(a, b store.Entity) int { return cmp.Compare(a.Version, b.Version) })
	return changes
}

func (s *Store) Touch(_ context.Context, chainID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.chains[chainID]; ok {
		c.state.LastSeen = s.Now()
	}
	return nil
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

func (s *Store) Collect(_ context.Context, r store.Retention) (store.Collected, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var done store.Collected
	for id, c := range s.chains {
		switch {
		case r.EmptyChainTTL > 0 && c.state.Version <= r.EmptyVersion && r.Now.Sub(c.created) > r.EmptyChainTTL:
			delete(s.chains, id)
			done.EmptyChains++
			continue
		case r.InactiveChainTTL > 0 && r.Now.Sub(c.state.LastSeen) > r.InactiveChainTTL:
			delete(s.chains, id)
			done.InactiveChains++
			continue
		}
		for entityID, e := range c.entities {
			expiring := r.ExpiringTypesTTL > 0 && slices.Contains(r.ExpiringTypes, e.DataType) &&
				r.Now.Sub(e.updated) > r.ExpiringTypesTTL
			purged := !expiring && r.TombstoneTTL > 0 && e.Deleted && r.Now.Sub(e.updated) > r.TombstoneTTL
			if !expiring && !purged {
				continue
			}
			c.state.Account(&e.Entity, nil)
			delete(c.entities, entityID)
			if expiring {
				done.Expired++
				continue
			}
			state := c.state.Types[e.DataType]
			state.PurgedVersion = max(state.PurgedVersion, e.Version)
			c.state.Types[e.DataType] = state
			done.Tombstones++
		}
	}
	return done, nil
}

// tx buffers writes until the store applies them, so a failed fn changes
// nothing. A nil write removes the entity.
type tx struct {
	chain   *chain
	state   store.Chain
	created bool
	writes  map[string]*store.Entity
	read    map[string]bool
}

func (t *tx) Chain() store.Chain { return t.state.Clone() }
func (t *tx) Created() bool      { return t.created }

func (t *tx) NextVersion() int64 {
	t.state.Version++
	return t.state.Version
}

func (t *tx) SetCustomPassphrase(custom bool) { t.state.CustomPassphrase = custom }

func (t *tx) Prefetch(context.Context, []string) error { return nil }

func (t *tx) Get(_ context.Context, id string) (*store.Entity, error) {
	t.read[id] = true
	if e, ok := t.writes[id]; ok {
		if e == nil {
			return nil, nil
		}
		copied := *e
		return &copied, nil
	}
	if e, ok := t.chain.entities[id]; ok {
		return &e.Entity, nil
	}
	return nil, nil
}

func (t *tx) Put(ctx context.Context, e store.Entity) error {
	old, _ := t.Get(ctx, e.ID)
	t.state.Account(old, &e)
	t.writes[e.ID] = &e
	return nil
}

func (t *tx) Evict(_ context.Context, dataType int32, entities, bytes int64) (int64, int64, error) {
	var candidates []store.Entity
	for id, e := range t.chain.entities {
		if _, written := t.writes[id]; e.DataType == dataType && !written && !t.read[id] {
			candidates = append(candidates, e.Entity)
		}
	}
	slices.SortFunc(candidates, func(a, b store.Entity) int { return cmp.Compare(a.Version, b.Version) })
	var n, size int64
	for _, e := range candidates {
		if n >= entities && size >= bytes {
			break
		}
		t.state.Account(&e, nil)
		t.writes[e.ID] = nil
		n++
		size += int64(len(e.Data))
	}
	return n, size, nil
}
