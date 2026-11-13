package memory

import (
	"bytes"
	"context"
	"slices"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

type account struct {
	store.Account
	envelopes []store.Envelope
	devices   []*device
}

type device struct {
	store.Device
	removed time.Time // zero while active
}

func (a *account) device(publicKey []byte) *device {
	for _, d := range a.devices {
		if bytes.Equal(d.PublicKey, publicKey) {
			return d
		}
	}
	return nil
}

func (a *account) active() int {
	n := 0
	for _, d := range a.devices {
		if d.removed.IsZero() {
			n++
		}
	}
	return n
}

func (s *Store) initAccounts() {
	if s.accounts == nil {
		s.accounts = map[string]*account{}
		s.claims = map[string]string{}
	}
}

func (s *Store) envelope(id string) (store.Envelope, bool) {
	for _, a := range s.accounts {
		for _, e := range a.envelopes {
			if e.ID == id {
				return e, true
			}
		}
	}
	return store.Envelope{}, false
}

func (s *Store) CreateAccount(_ context.Context, a store.Account, e store.Envelope, d store.Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAccounts()
	if _, taken := s.envelope(e.ID); taken {
		return store.ErrEnvelopeTaken
	}
	now := s.Now()
	a.CreatedAt, e.AccountID, e.UpdatedAt = now, a.ID, now
	d.AccountID, d.CreatedAt, d.LastSeen = a.ID, now, now
	s.accounts[a.ID] = &account{Account: a, envelopes: []store.Envelope{e}, devices: []*device{{Device: d}}}
	return nil
}

func (s *Store) Account(_ context.Context, id string) (store.Account, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accounts[id]; ok {
		return a.Account, true, nil
	}
	return store.Account{}, false, nil
}

func (s *Store) Envelope(_ context.Context, id string) (store.Envelope, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.envelope(id)
	return e, ok, nil
}

func (s *Store) AccountEnvelope(_ context.Context, accountID, scheme string) (store.Envelope, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accounts[accountID]; ok {
		for _, e := range a.envelopes {
			if e.Scheme == scheme {
				return e, true, nil
			}
		}
	}
	return store.Envelope{}, false, nil
}

func (s *Store) AddDevice(_ context.Context, d store.Device, maxDevices int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[d.AccountID]
	if !ok {
		return store.ErrNoAccount
	}
	now := s.Now()
	if old := a.device(d.PublicKey); old != nil {
		if !old.removed.IsZero() {
			return store.ErrDeviceRemoved
		}
		old.Name, old.LastSeen = d.Name, now
		return nil
	}
	if maxDevices > 0 && a.active() >= maxDevices {
		return store.ErrTooManyDevices
	}
	d.CreatedAt, d.LastSeen = now, now
	a.devices = append(a.devices, &device{Device: d})
	return nil
}

func (s *Store) UseDevice(_ context.Context, accountID string, publicKey []byte, interval time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[accountID]
	if !ok {
		return false, nil
	}
	d := a.device(publicKey)
	if d == nil || !d.removed.IsZero() {
		return false, nil
	}
	if now := s.Now(); now.Sub(d.LastSeen) >= interval {
		d.LastSeen = now
	}
	return true, nil
}

func (s *Store) Devices(_ context.Context, accountID string) ([]store.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Device
	if a, ok := s.accounts[accountID]; ok {
		for _, d := range a.devices {
			if d.removed.IsZero() {
				out = append(out, d.Device)
			}
		}
	}
	return out, nil
}

func (s *Store) RemoveDevice(_ context.Context, accountID string, publicKey []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[accountID]
	if !ok {
		return store.ErrNoDevice
	}
	d := a.device(publicKey)
	switch {
	case d == nil || !d.removed.IsZero():
		return store.ErrNoDevice
	case a.active() <= 1:
		return store.ErrLastDevice
	}
	d.removed = s.Now()
	return nil
}

func (s *Store) ChainAccount(_ context.Context, chainID string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, ok := s.claims[chainID]
	return owner, ok, nil
}

func (s *Store) ClaimChain(_ context.Context, accountID, chainID string, maxChains int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAccounts()
	if _, ok := s.accounts[accountID]; !ok {
		return store.ErrNoAccount
	}
	if owner, ok := s.claims[chainID]; ok {
		if owner == accountID {
			return nil
		}
		return store.ErrChainTaken
	}
	if _, exists := s.chains[chainID]; exists {
		return store.ErrChainTaken
	}
	n := 0
	for _, owner := range s.claims {
		if owner == accountID {
			n++
		}
	}
	if maxChains > 0 && n >= maxChains {
		return store.ErrTooManyChains
	}
	s.claims[chainID] = accountID
	return nil
}

func (s *Store) DeleteAccount(_ context.Context, accountID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[accountID]; !ok {
		return nil, nil
	}
	var removed []string
	for chainID, owner := range s.claims {
		if owner != accountID {
			continue
		}
		delete(s.claims, chainID)
		if _, ok := s.chains[chainID]; ok {
			delete(s.chains, chainID)
			removed = append(removed, chainID)
		}
	}
	delete(s.accounts, accountID)
	slices.Sort(removed)
	return removed, nil
}

// collectDevices removes the records of devices removed before cutoff.
// s.mu is held.
func (s *Store) collectDevices(cutoff time.Time) int64 {
	var n int64
	for _, a := range s.accounts {
		a.devices = slices.DeleteFunc(a.devices, func(d *device) bool {
			gone := !d.removed.IsZero() && d.removed.Before(cutoff)
			if gone {
				n++
			}
			return gone
		})
	}
	return n
}

func (s *Store) CountAccounts(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.accounts)), nil
}
