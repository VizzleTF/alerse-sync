package memory

import (
	"context"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

// live returns the slot if it has not expired and its chain exists. Call
// with s.mu held.
func (s *Store) live(id string, ttl time.Duration) (*store.PairSlot, bool) {
	slot, ok := s.slots[id]
	if !ok || s.Now().Sub(slot.Created) >= ttl {
		return nil, false
	}
	if _, chain := s.chains[slot.ChainID]; !chain {
		return nil, false
	}
	return slot, true
}

func (s *Store) CreatePairSlot(_ context.Context, slot store.PairSlot, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slots == nil {
		s.slots = map[string]*store.PairSlot{}
	}
	for id := range s.slots {
		if _, ok := s.live(id, ttl); !ok {
			delete(s.slots, id)
		}
	}
	if _, ok := s.chains[slot.ChainID]; !ok {
		return store.ErrNoChain
	}
	if _, ok := s.slots[slot.ID]; ok {
		return store.ErrSlotTaken
	}
	slot.Created = s.Now()
	s.slots[slot.ID] = &slot
	return nil
}

func (s *Store) PairSlot(_ context.Context, id string, ttl time.Duration) (store.PairSlot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot, ok := s.live(id, ttl); ok {
		return *slot, true, nil
	}
	return store.PairSlot{}, false, nil
}

func (s *Store) JoinPairSlot(_ context.Context, id string, msg []byte, name string, confirm []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, ok := s.live(id, ttl)
	switch {
	case !ok:
		return store.ErrNoSlot
	case slot.BMsg != nil:
		return store.ErrSlotUsed
	}
	slot.BMsg, slot.BName, slot.BConfirm = msg, name, confirm
	return nil
}

func (s *Store) ApprovePairSlot(_ context.Context, id, chainID string, box []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, ok := s.live(id, ttl)
	switch {
	case !ok || slot.ChainID != chainID:
		return store.ErrNoSlot
	case slot.BMsg == nil || slot.Box != nil:
		return store.ErrSlotUsed
	}
	slot.Box = box
	return nil
}

func (s *Store) DeletePairSlot(_ context.Context, id, chainID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot, ok := s.slots[id]; ok && slot.ChainID == chainID {
		delete(s.slots, id)
	}
	return nil
}
