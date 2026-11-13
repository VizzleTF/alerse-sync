package postgres

import (
	"context"
	"crypto/rand"
	"fmt"
)

func (s *Store) ServerSecret(ctx context.Context, name string) ([]byte, error) {
	value := make([]byte, 32)
	_, _ = rand.Read(value) // never fails
	// The first instance to ask stores its value; every instance reads the
	// stored one.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO server_secrets (name, value) VALUES ($1, $2) ON CONFLICT DO NOTHING`, name, value); err != nil {
		return nil, fmt.Errorf("store server secret: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT value FROM server_secrets WHERE name = $1`, name).Scan(&value); err != nil {
		return nil, fmt.Errorf("read server secret: %w", err)
	}
	return value, nil
}
