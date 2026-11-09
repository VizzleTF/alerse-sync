package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

const loginColumns = `id, chain_id, scheme, salt, kdf, verifier, verifier_salt, secret, updated_at`

func (s *Store) readLogin(ctx context.Context, where string, arg string) (store.Login, bool, error) {
	var l store.Login
	err := s.pool.QueryRow(ctx, `SELECT `+loginColumns+` FROM logins WHERE `+where+` = $1`, arg).Scan(
		&l.ID, &l.ChainID, &l.Scheme, &l.Salt, &l.KDF, &l.Verifier, &l.VerifierSalt, &l.Secret, &l.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Login{}, false, nil
	}
	if err != nil {
		return store.Login{}, false, fmt.Errorf("read login: %w", err)
	}
	return l, true, nil
}

func (s *Store) Login(ctx context.Context, id string) (store.Login, bool, error) {
	return s.readLogin(ctx, "id", id)
}

func (s *Store) ChainLogin(ctx context.Context, chainID string) (store.Login, bool, error) {
	return s.readLogin(ctx, "chain_id", chainID)
}

func (s *Store) SetLogin(ctx context.Context, l store.Login) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var owner string
		err := tx.QueryRow(ctx, `SELECT chain_id FROM logins WHERE id = $1`, l.ID).Scan(&owner)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		case owner != l.ChainID:
			return store.ErrLoginTaken
		}
		if _, err := tx.Exec(ctx, `DELETE FROM logins WHERE chain_id = $1`, l.ChainID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO logins (`+loginColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
			l.ID, l.ChainID, l.Scheme, l.Salt, l.KDF, l.Verifier, l.VerifierSalt, l.Secret)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation: another chain took the ID meanwhile
			return store.ErrLoginTaken
		case "23503": // foreign_key_violation
			return store.ErrNoChain
		}
	}
	if err != nil && !errors.Is(err, store.ErrLoginTaken) {
		return fmt.Errorf("set login: %w", err)
	}
	return err
}

func (s *Store) DeleteLogin(ctx context.Context, chainID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM logins WHERE chain_id = $1`, chainID); err != nil {
		return fmt.Errorf("delete login: %w", err)
	}
	return nil
}

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
