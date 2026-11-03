// Package postgres stores sync chains in PostgreSQL.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is a store.Store on a pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a store on pool. Call Migrate before using it.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// migrationLock serializes migrations of server instances starting together.
const migrationLock = 0x6172636f6d69756d // "arcomium"

// Migrate brings the schema up to date.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("lock schema: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock) //nolint:errcheck // the lock ends with the session anyway

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(files)
	for _, file := range files {
		number, _, _ := strings.Cut(strings.TrimPrefix(file, "migrations/"), "_")
		version, err := strconv.Atoi(number)
		if err != nil {
			return fmt.Errorf("migration %s: bad name", file)
		}
		if version <= current {
			continue
		}
		sql, err := migrations.ReadFile(file)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", file, err)
		}
	}
	return nil
}

// Ping checks the database connection.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) Chain(ctx context.Context, chainID string) (store.Chain, bool, error) {
	chain := store.Chain{ID: chainID}
	err := s.pool.QueryRow(ctx,
		`SELECT birthday, keystore_key, version, live_entities FROM chains WHERE id = $1`, chainID,
	).Scan(&chain.Birthday, &chain.KeystoreKey, &chain.Version, &chain.LiveEntities)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Chain{}, false, nil
	}
	if err != nil {
		return store.Chain{}, false, fmt.Errorf("read chain: %w", err)
	}
	return chain, true, nil
}

func (s *Store) Update(ctx context.Context, chainID string, create bool, fn func(store.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(pgTx pgx.Tx) error {
		tx := &tx{tx: pgTx, chain: store.Chain{ID: chainID}}
		if create {
			birthday, key := store.NewChainSecrets()
			tag, err := pgTx.Exec(ctx,
				`INSERT INTO chains (id, birthday, keystore_key) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
				chainID, birthday, key)
			if err != nil {
				return fmt.Errorf("create chain: %w", err)
			}
			tx.created = tag.RowsAffected() == 1
		}
		err := pgTx.QueryRow(ctx,
			`SELECT birthday, keystore_key, version, live_entities FROM chains WHERE id = $1 FOR UPDATE`, chainID,
		).Scan(&tx.chain.Birthday, &tx.chain.KeystoreKey, &tx.chain.Version, &tx.chain.LiveEntities)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNoChain
		}
		if err != nil {
			return fmt.Errorf("lock chain: %w", err)
		}
		start := tx.chain

		if err := fn(tx); err != nil {
			return err
		}
		if tx.chain.Version == start.Version && tx.chain.LiveEntities == start.LiveEntities {
			return nil
		}
		_, err = pgTx.Exec(ctx,
			`UPDATE chains SET version = $2, live_entities = $3, updated_at = now() WHERE id = $1`,
			chainID, tx.chain.Version, tx.chain.LiveEntities)
		if err != nil {
			return fmt.Errorf("update chain: %w", err)
		}
		return nil
	})
}

func (s *Store) ChangesSince(ctx context.Context, chainID string, change store.Change, limit int) ([]store.Entity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, data_type, version, deleted, server_tag, data FROM entities
		WHERE chain_id = $1 AND data_type = $2 AND version > $3
		ORDER BY version LIMIT $4`,
		chainID, change.DataType, change.After, limit)
	if err != nil {
		return nil, fmt.Errorf("read changes: %w", err)
	}
	entities, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Entity, error) {
		var e store.Entity
		err := row.Scan(&e.ID, &e.DataType, &e.Version, &e.Deleted, &e.ServerTag, &e.Data)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("read changes: %w", err)
	}
	return entities, nil
}

func (s *Store) CountSince(ctx context.Context, chainID string, change store.Change) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE chain_id = $1 AND data_type = $2 AND version > $3`,
		chainID, change.DataType, change.After,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count changes: %w", err)
	}
	return count, nil
}

func (s *Store) Delete(ctx context.Context, chainID, birthday string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM chains WHERE id = $1 AND birthday = $2`, chainID, birthday); err != nil {
		return fmt.Errorf("delete chain: %w", err)
	}
	return nil
}

func (s *Store) CountChains(ctx context.Context) (int64, error) {
	var count int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM chains`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count chains: %w", err)
	}
	return count, nil
}

type tx struct {
	tx      pgx.Tx
	chain   store.Chain
	created bool
}

func (t *tx) Chain() store.Chain { return t.chain }
func (t *tx) Created() bool      { return t.created }

func (t *tx) NextVersion() int64 {
	t.chain.Version++
	return t.chain.Version
}

func (t *tx) Get(ctx context.Context, id string) (*store.Entity, error) {
	e := store.Entity{ID: id}
	err := t.tx.QueryRow(ctx,
		`SELECT data_type, version, deleted, server_tag, data FROM entities WHERE chain_id = $1 AND id = $2`,
		t.chain.ID, id,
	).Scan(&e.DataType, &e.Version, &e.Deleted, &e.ServerTag, &e.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read entity: %w", err)
	}
	return &e, nil
}

func (t *tx) Put(ctx context.Context, e store.Entity) error {
	// old reads the row as it was before this statement.
	var wasDeleted *bool
	err := t.tx.QueryRow(ctx, `
		WITH old AS (SELECT deleted FROM entities WHERE chain_id = $1 AND id = $2)
		INSERT INTO entities (chain_id, id, data_type, version, deleted, server_tag, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (chain_id, id) DO UPDATE SET
			data_type = excluded.data_type, version = excluded.version, deleted = excluded.deleted,
			server_tag = excluded.server_tag, data = excluded.data
		RETURNING (SELECT deleted FROM old)`,
		t.chain.ID, e.ID, e.DataType, e.Version, e.Deleted, e.ServerTag, e.Data,
	).Scan(&wasDeleted)
	if err != nil {
		return fmt.Errorf("write entity: %w", err)
	}
	wasLive := wasDeleted != nil && !*wasDeleted
	switch {
	case wasLive && e.Deleted:
		t.chain.LiveEntities--
	case !wasLive && !e.Deleted:
		t.chain.LiveEntities++
	}
	return nil
}
