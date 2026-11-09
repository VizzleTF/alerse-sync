package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

func (s *Store) CreatePairSlot(ctx context.Context, slot store.PairSlot, ttl time.Duration) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM pair_slots WHERE created_at <= now() - $1::interval`, ttl); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO pair_slots (id, chain_id, a_msg) VALUES ($1, $2, $3)`,
			slot.ID, slot.ChainID, slot.AMsg)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return store.ErrSlotTaken
		case "23503": // foreign_key_violation
			return store.ErrNoChain
		}
	}
	if err != nil {
		return fmt.Errorf("create pairing slot: %w", err)
	}
	return nil
}

func (s *Store) PairSlot(ctx context.Context, id string, ttl time.Duration) (store.PairSlot, bool, error) {
	slot := store.PairSlot{ID: id}
	var name *string
	err := s.pool.QueryRow(ctx, `SELECT chain_id, a_msg, b_msg, b_name, b_confirm, box, created_at
		FROM pair_slots WHERE id = $1 AND created_at > now() - $2::interval`, id, ttl).Scan(
		&slot.ChainID, &slot.AMsg, &slot.BMsg, &name, &slot.BConfirm, &slot.Box, &slot.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.PairSlot{}, false, nil
	}
	if err != nil {
		return store.PairSlot{}, false, fmt.Errorf("read pairing slot: %w", err)
	}
	if name != nil {
		slot.BName = *name
	}
	return slot, true, nil
}

// step runs a conditional update and tells a missing slot from one in the
// wrong state.
func (s *Store) step(ctx context.Context, id string, ttl time.Duration, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("update pairing slot: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	_, ok, err := s.PairSlot(ctx, id, ttl)
	switch {
	case err != nil:
		return err
	case !ok:
		return store.ErrNoSlot
	}
	return store.ErrSlotUsed
}

func (s *Store) JoinPairSlot(ctx context.Context, id string, msg []byte, name string, confirm []byte, ttl time.Duration) error {
	return s.step(ctx, id, ttl, `UPDATE pair_slots SET b_msg = $3, b_name = $4, b_confirm = $5
		WHERE id = $1 AND created_at > now() - $2::interval AND b_msg IS NULL`, id, ttl, msg, name, confirm)
}

func (s *Store) ApprovePairSlot(ctx context.Context, id, chainID string, box []byte, ttl time.Duration) error {
	slot, ok, err := s.PairSlot(ctx, id, ttl)
	if err != nil {
		return err
	}
	if !ok || slot.ChainID != chainID {
		return store.ErrNoSlot
	}
	return s.step(ctx, id, ttl, `UPDATE pair_slots SET box = $3
		WHERE id = $1 AND created_at > now() - $2::interval AND b_msg IS NOT NULL AND box IS NULL`, id, ttl, box)
}

func (s *Store) DeletePairSlot(ctx context.Context, id, chainID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM pair_slots WHERE id = $1 AND chain_id = $2`, id, chainID); err != nil {
		return fmt.Errorf("delete pairing slot: %w", err)
	}
	return nil
}
