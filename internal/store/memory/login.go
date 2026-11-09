package memory

import (
	"context"
	"crypto/rand"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

func (s *Store) Login(_ context.Context, id string) (store.Login, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.chains {
		if c.login != nil && c.login.ID == id {
			return *c.login, true, nil
		}
	}
	return store.Login{}, false, nil
}

func (s *Store) ChainLogin(_ context.Context, chainID string) (store.Login, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.chains[chainID]; ok && c.login != nil {
		return *c.login, true, nil
	}
	return store.Login{}, false, nil
}

func (s *Store) SetLogin(_ context.Context, l store.Login) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chains[l.ChainID]
	if !ok {
		return store.ErrNoChain
	}
	for id, other := range s.chains {
		if id != l.ChainID && other.login != nil && other.login.ID == l.ID {
			return store.ErrLoginTaken
		}
	}
	l.UpdatedAt = s.Now()
	c.login = &l
	return nil
}

func (s *Store) DeleteLogin(_ context.Context, chainID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.chains[chainID]; ok {
		c.login = nil
	}
	return nil
}

func (s *Store) ServerSecret(_ context.Context, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secrets == nil {
		s.secrets = map[string][]byte{}
	}
	if v, ok := s.secrets[name]; ok {
		return v, nil
	}
	v := make([]byte, 32)
	_, _ = rand.Read(v)
	s.secrets[name] = v
	return v, nil
}
