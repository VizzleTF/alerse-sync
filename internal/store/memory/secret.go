package memory

import (
	"context"
	"crypto/rand"
)

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
