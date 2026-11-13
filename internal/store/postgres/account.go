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

const envelopeColumns = `id, account_id, scheme, salt, kdf, verifier, verifier_salt, wrapped, updated_at`

func (s *Store) CreateAccount(ctx context.Context, a store.Account, e store.Envelope, d store.Device) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, kind) VALUES ($1, $2)`, a.ID, a.Kind); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO envelopes (id, account_id, scheme, salt, kdf, verifier, verifier_salt, wrapped)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			e.ID, a.ID, e.Scheme, e.Salt, e.KDF, e.Verifier, e.VerifierSalt, e.Wrapped); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO devices (account_id, public_key, name) VALUES ($1, $2, $3)`,
			a.ID, d.PublicKey, d.Name)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.TableName == "envelopes" {
		return store.ErrEnvelopeTaken
	}
	if err != nil {
		return fmt.Errorf("create account: %w", err)
	}
	return nil
}

func (s *Store) Account(ctx context.Context, id string) (store.Account, bool, error) {
	a := store.Account{ID: id}
	err := s.pool.QueryRow(ctx, `SELECT kind, created_at FROM accounts WHERE id = $1`, id).Scan(&a.Kind, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Account{}, false, nil
	}
	if err != nil {
		return store.Account{}, false, fmt.Errorf("read account: %w", err)
	}
	return a, true, nil
}

func (s *Store) readEnvelope(ctx context.Context, where string, args ...any) (store.Envelope, bool, error) {
	var e store.Envelope
	err := s.pool.QueryRow(ctx, `SELECT `+envelopeColumns+` FROM envelopes WHERE `+where, args...).Scan(
		&e.ID, &e.AccountID, &e.Scheme, &e.Salt, &e.KDF, &e.Verifier, &e.VerifierSalt, &e.Wrapped, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Envelope{}, false, nil
	}
	if err != nil {
		return store.Envelope{}, false, fmt.Errorf("read envelope: %w", err)
	}
	return e, true, nil
}

func (s *Store) Envelope(ctx context.Context, id string) (store.Envelope, bool, error) {
	return s.readEnvelope(ctx, `id = $1`, id)
}

func (s *Store) AccountEnvelope(ctx context.Context, accountID, scheme string) (store.Envelope, bool, error) {
	return s.readEnvelope(ctx, `account_id = $1 AND scheme = $2`, accountID, scheme)
}

// lockAccount takes the account's lock for the rest of tx, which serializes
// the changes that count its devices or chains.
func lockAccount(ctx context.Context, tx pgx.Tx, accountID string) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, accountID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNoAccount
	}
	return err
}

func (s *Store) AddDevice(ctx context.Context, d store.Device, maxDevices int) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockAccount(ctx, tx, d.AccountID); err != nil {
			return err
		}
		var removed *time.Time
		err := tx.QueryRow(ctx, `SELECT removed_at FROM devices WHERE account_id = $1 AND public_key = $2`,
			d.AccountID, d.PublicKey).Scan(&removed)
		switch {
		case err == nil && removed != nil:
			return store.ErrDeviceRemoved
		case err == nil:
			_, err = tx.Exec(ctx, `UPDATE devices SET name = $3, last_seen_at = now()
				WHERE account_id = $1 AND public_key = $2`, d.AccountID, d.PublicKey, d.Name)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if maxDevices > 0 {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM devices WHERE account_id = $1 AND removed_at IS NULL`,
				d.AccountID).Scan(&n); err != nil {
				return err
			}
			if n >= maxDevices {
				return store.ErrTooManyDevices
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO devices (account_id, public_key, name) VALUES ($1, $2, $3)`,
			d.AccountID, d.PublicKey, d.Name)
		return err
	})
	if err != nil && !isStoreError(err) {
		return fmt.Errorf("add device: %w", err)
	}
	return err
}

func (s *Store) UseDevice(ctx context.Context, accountID string, publicKey []byte, interval time.Duration) (bool, error) {
	// One round trip: the update runs whether or not the select finds the
	// device, and touches it only when it was not seen for interval.
	var active bool
	err := s.pool.QueryRow(ctx, `
		WITH touched AS (
			UPDATE devices SET last_seen_at = now()
			WHERE account_id = $1 AND public_key = $2 AND removed_at IS NULL
				AND last_seen_at < now() - make_interval(secs => $3)
			RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM devices WHERE account_id = $1 AND public_key = $2 AND removed_at IS NULL)`,
		accountID, publicKey, interval.Seconds()).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check device: %w", err)
	}
	return active, nil
}

func (s *Store) Devices(ctx context.Context, accountID string) ([]store.Device, error) {
	rows, err := s.pool.Query(ctx, `SELECT account_id, public_key, name, created_at, last_seen_at FROM devices
		WHERE account_id = $1 AND removed_at IS NULL ORDER BY created_at, public_key`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	devices, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Device, error) {
		var d store.Device
		err := row.Scan(&d.AccountID, &d.PublicKey, &d.Name, &d.CreatedAt, &d.LastSeen)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	return devices, nil
}

func (s *Store) RemoveDevice(ctx context.Context, accountID string, publicKey []byte) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockAccount(ctx, tx, accountID); err != nil {
			if errors.Is(err, store.ErrNoAccount) {
				return store.ErrNoDevice
			}
			return err
		}
		var target, active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE public_key = $2), count(*) FROM devices
			WHERE account_id = $1 AND removed_at IS NULL`, accountID, publicKey).Scan(&target, &active); err != nil {
			return err
		}
		switch {
		case target == 0:
			return store.ErrNoDevice
		case active <= 1:
			return store.ErrLastDevice
		}
		_, err := tx.Exec(ctx, `UPDATE devices SET removed_at = now() WHERE account_id = $1 AND public_key = $2`,
			accountID, publicKey)
		return err
	})
	if err != nil && !isStoreError(err) {
		return fmt.Errorf("remove device: %w", err)
	}
	return err
}

func (s *Store) ChainAccount(ctx context.Context, chainID string) (string, bool, error) {
	var accountID string
	err := s.pool.QueryRow(ctx, `SELECT account_id FROM account_chains WHERE chain_id = $1`, chainID).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read chain account: %w", err)
	}
	return accountID, true, nil
}

func (s *Store) ClaimChain(ctx context.Context, accountID, chainID string, maxChains int) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		var owner string
		err := tx.QueryRow(ctx, `SELECT account_id FROM account_chains WHERE chain_id = $1`, chainID).Scan(&owner)
		switch {
		case err == nil && owner == accountID:
			return nil
		case err == nil:
			return store.ErrChainTaken
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var exists bool
		var n int
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chains WHERE id = $1),
			(SELECT count(*) FROM account_chains WHERE account_id = $2)`, chainID, accountID).Scan(&exists, &n); err != nil {
			return err
		}
		switch {
		case exists:
			return store.ErrChainTaken
		case maxChains > 0 && n >= maxChains:
			return store.ErrTooManyChains
		}
		tag, err := tx.Exec(ctx, `INSERT INTO account_chains (chain_id, account_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, chainID, accountID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Another account claimed it meanwhile.
			return store.ErrChainTaken
		}
		return nil
	})
	if err != nil && !isStoreError(err) {
		return fmt.Errorf("claim chain: %w", err)
	}
	return err
}

func (s *Store) DeleteAccount(ctx context.Context, accountID string) ([]string, error) {
	var removed []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		// The chains under their locks, so that no commit is half way through
		// one; their entities, sign-ins and pairings go with them.
		rows, err := tx.Query(ctx, `SELECT c.id FROM chains c JOIN account_chains a ON a.chain_id = c.id
			WHERE a.account_id = $1 ORDER BY c.id FOR UPDATE OF c`, accountID)
		if err != nil {
			return err
		}
		if removed, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM chains WHERE id = ANY($1)`, removed); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, accountID)
		return err
	})
	if errors.Is(err, store.ErrNoAccount) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delete account: %w", err)
	}
	return removed, nil
}

func (s *Store) CountAccounts(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count accounts: %w", err)
	}
	return n, nil
}

// collectDevices removes the records of devices removed before cutoff.
func (s *Store) collectDevices(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM devices WHERE removed_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// isStoreError reports whether err is one of the store's own answers, which
// callers check with errors.Is.
func isStoreError(err error) bool {
	for _, e := range []error{
		store.ErrNoAccount, store.ErrNoDevice, store.ErrDeviceRemoved,
		store.ErrTooManyDevices, store.ErrLastDevice, store.ErrChainTaken, store.ErrTooManyChains,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
