// Package postgres stores sync chains in PostgreSQL.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math"
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

// chainColumns are the columns readChain reads, of chains c and chain_types t.
const chainColumns = `c.birthday, c.keystore_key, c.version, c.live_entities, c.bytes_used,
	c.custom_passphrase, c.last_seen_at,
	t.data_type, t.version, t.purged_version, t.entities, t.tombstones, t.bytes`

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// readChain reads a chain with its data types in one round trip. lock takes
// the chain's lock.
func readChain(ctx context.Context, q querier, chainID string, lock bool) (store.Chain, bool, error) {
	sql := `SELECT ` + chainColumns + ` FROM chains c LEFT JOIN chain_types t ON t.chain_id = c.id WHERE c.id = $1`
	if lock {
		sql += ` FOR UPDATE OF c`
	}
	rows, err := q.Query(ctx, sql, chainID)
	if err != nil {
		return store.Chain{}, false, err
	}
	defer rows.Close()
	chain := store.Chain{ID: chainID, Types: map[int32]store.TypeState{}}
	found := false
	for rows.Next() {
		var dataType *int32
		var version, purged, entities, tombstones, bytes *int64
		if err := rows.Scan(&chain.Birthday, &chain.KeystoreKey, &chain.Version, &chain.LiveEntities, &chain.Bytes,
			&chain.CustomPassphrase, &chain.LastSeen,
			&dataType, &version, &purged, &entities, &tombstones, &bytes); err != nil {
			return store.Chain{}, false, err
		}
		found = true
		if dataType != nil {
			chain.Types[*dataType] = store.TypeState{
				Version: *version, PurgedVersion: *purged,
				Entities: *entities, Tombstones: *tombstones, Bytes: *bytes,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return store.Chain{}, false, err
	}
	if !found {
		return store.Chain{}, false, nil
	}
	return chain, true, nil
}

func (s *Store) Chain(ctx context.Context, chainID string) (store.Chain, bool, error) {
	chain, ok, err := readChain(ctx, s.pool, chainID, false)
	if err != nil {
		return store.Chain{}, false, fmt.Errorf("read chain: %w", err)
	}
	return chain, ok, nil
}

func (s *Store) Update(ctx context.Context, chainID string, fn func(store.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(pgTx pgx.Tx) error {
		return run(ctx, pgTx, chainID, false, fn)
	})
}

func (s *Store) Create(ctx context.Context, chainID string, maxChains int64, fn func(store.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(pgTx pgx.Tx) error {
		birthday, key := store.NewChainSecrets()
		tag, err := pgTx.Exec(ctx,
			`INSERT INTO chains (id, birthday, keystore_key) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			chainID, birthday, key)
		if err != nil {
			return fmt.Errorf("create chain: %w", err)
		}
		created := tag.RowsAffected() == 1
		if created && maxChains > 0 {
			// The insert's trigger counted the chain and holds the counter's
			// lock until this transaction ends, so creations do not race.
			var n int64
			if err := pgTx.QueryRow(ctx, `SELECT n FROM chain_count`).Scan(&n); err != nil {
				return fmt.Errorf("count chains: %w", err)
			}
			if n > maxChains {
				return store.ErrFull
			}
		}
		return run(ctx, pgTx, chainID, created, fn)
	})
}

// run locks the chain, runs fn and writes what it changed.
func run(ctx context.Context, pgTx pgx.Tx, chainID string, created bool, fn func(store.Tx) error) error {
	chain, ok, err := readChain(ctx, pgTx, chainID, true)
	if err != nil {
		return fmt.Errorf("lock chain: %w", err)
	}
	if !ok {
		return store.ErrNoChain
	}
	t := &tx{tx: pgTx, chain: chain, start: chain.Clone(), created: created, known: map[string]*store.Entity{}}
	if err := fn(t); err != nil {
		return err
	}
	return t.flush(ctx)
}

func (s *Store) ChangesSince(ctx context.Context, chainID string, change store.Change, limit int) ([]store.Entity, error) {
	sql := `SELECT id, data_type, version, deleted, server_tag, data FROM entities
		WHERE chain_id = $1 AND data_type = $2 AND version > $3 AND version <= $4`
	if change.LiveOnly {
		sql += ` AND NOT deleted`
	}
	rows, err := s.pool.Query(ctx, sql+` ORDER BY version LIMIT $5`,
		chainID, change.DataType, change.After, until(change), limit)
	if err != nil {
		return nil, fmt.Errorf("read changes: %w", err)
	}
	entities, err := pgx.CollectRows(rows, scanEntity)
	if err != nil {
		return nil, fmt.Errorf("read changes: %w", err)
	}
	return entities, nil
}

func scanEntity(row pgx.CollectableRow) (store.Entity, error) {
	var e store.Entity
	err := row.Scan(&e.ID, &e.DataType, &e.Version, &e.Deleted, &e.ServerTag, &e.Data)
	return e, err
}

func (s *Store) CountSince(ctx context.Context, chainID string, change store.Change, limit int) (int64, error) {
	sql := `SELECT 1 FROM entities WHERE chain_id = $1 AND data_type = $2 AND version > $3 AND version <= $4`
	if change.LiveOnly {
		sql += ` AND NOT deleted`
	}
	var count int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM (`+sql+` LIMIT $5) t`,
		chainID, change.DataType, change.After, until(change), limit,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count changes: %w", err)
	}
	return count, nil
}

func until(change store.Change) int64 {
	if change.Until == 0 {
		return math.MaxInt64
	}
	return change.Until
}

func (s *Store) Touch(ctx context.Context, chainID string) error {
	if _, err := s.pool.Exec(ctx, `UPDATE chains SET last_seen_at = now() WHERE id = $1`, chainID); err != nil {
		return fmt.Errorf("touch chain: %w", err)
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, chainID, birthday string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM chains WHERE id = $1 AND birthday = $2`, chainID, birthday); err != nil {
		return fmt.Errorf("delete chain: %w", err)
	}
	return nil
}

func (s *Store) CountChains(ctx context.Context) (int64, error) {
	var count int64
	if err := s.pool.QueryRow(ctx, `SELECT n FROM chain_count`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count chains: %w", err)
	}
	return count, nil
}

// tx is a transaction on a locked chain. It remembers the entities it read
// and wrote, and sends its writes in one batch when fn is done.
type tx struct {
	tx      pgx.Tx
	chain   store.Chain
	start   store.Chain
	created bool
	// known holds the entities read or written; nil means there is none.
	known  map[string]*store.Entity
	writes pgx.Batch
}

func (t *tx) Chain() store.Chain              { return t.chain.Clone() }
func (t *tx) Created() bool                   { return t.created }
func (t *tx) SetCustomPassphrase(custom bool) { t.chain.CustomPassphrase = custom }

func (t *tx) NextVersion() int64 {
	t.chain.Version++
	return t.chain.Version
}

func (t *tx) Prefetch(ctx context.Context, ids []string) error {
	var missing []string
	for _, id := range ids {
		if _, ok := t.known[id]; !ok && !slices.Contains(missing, id) {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	rows, err := t.tx.Query(ctx,
		`SELECT id, data_type, version, deleted, server_tag, data FROM entities WHERE chain_id = $1 AND id = ANY($2)`,
		t.chain.ID, missing)
	if err != nil {
		return fmt.Errorf("read entities: %w", err)
	}
	found, err := pgx.CollectRows(rows, scanEntity)
	if err != nil {
		return fmt.Errorf("read entities: %w", err)
	}
	for _, id := range missing {
		t.known[id] = nil
	}
	for i := range found {
		t.known[found[i].ID] = &found[i]
	}
	return nil
}

func (t *tx) Get(ctx context.Context, id string) (*store.Entity, error) {
	if e, ok := t.known[id]; ok {
		if e == nil {
			return nil, nil
		}
		copied := *e
		return &copied, nil
	}
	e := store.Entity{ID: id}
	err := t.tx.QueryRow(ctx,
		`SELECT data_type, version, deleted, server_tag, data FROM entities WHERE chain_id = $1 AND id = $2`,
		t.chain.ID, id,
	).Scan(&e.DataType, &e.Version, &e.Deleted, &e.ServerTag, &e.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		t.known[id] = nil
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read entity: %w", err)
	}
	t.known[id] = &e
	copied := e
	return &copied, nil
}

func (t *tx) Put(ctx context.Context, e store.Entity) error {
	old, err := t.Get(ctx, e.ID)
	if err != nil {
		return err
	}
	t.chain.Account(old, &e)
	t.known[e.ID] = &e
	t.writes.Queue(`
		INSERT INTO entities (chain_id, id, data_type, version, deleted, server_tag, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (chain_id, id) DO UPDATE SET
			data_type = excluded.data_type, version = excluded.version, deleted = excluded.deleted,
			server_tag = excluded.server_tag, data = excluded.data, updated_at = now()`,
		t.chain.ID, e.ID, e.DataType, e.Version, e.Deleted, e.ServerTag, e.Data)
	return nil
}

// evictBatch is the most entities Evict removes in one statement.
const evictBatch = 500

func (t *tx) Evict(ctx context.Context, dataType int32, entities, bytes int64) (int64, int64, error) {
	keep := make([]string, 0, len(t.known))
	for id := range t.known {
		keep = append(keep, id)
	}
	var n, size int64
	for n < entities || size < bytes {
		limit := int64(evictBatch)
		if size >= bytes {
			limit = min(limit, entities-n)
		}
		rows, err := t.tx.Query(ctx, `
			DELETE FROM entities WHERE chain_id = $1 AND id IN (
				SELECT id FROM entities WHERE chain_id = $1 AND data_type = $2 AND NOT (id = ANY($3))
				ORDER BY version LIMIT $4)
			RETURNING deleted, octet_length(data)`,
			t.chain.ID, dataType, keep, limit)
		if err != nil {
			return n, size, fmt.Errorf("evict entities: %w", err)
		}
		var removed int64
		for rows.Next() {
			var deleted bool
			var length int64
			if err := rows.Scan(&deleted, &length); err != nil {
				rows.Close()
				return n, size, fmt.Errorf("evict entities: %w", err)
			}
			t.chain.Remove(dataType, deleted, length)
			n++
			size += length
			removed++
		}
		if err := rows.Err(); err != nil {
			return n, size, fmt.Errorf("evict entities: %w", err)
		}
		if removed < limit {
			break
		}
	}
	return n, size, nil
}

// flush writes the queued entities and the chain's new state.
func (t *tx) flush(ctx context.Context) error {
	for dataType, state := range t.chain.Types {
		if state == t.start.Types[dataType] {
			continue
		}
		t.writes.Queue(`
			INSERT INTO chain_types (chain_id, data_type, version, entities, tombstones, bytes)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (chain_id, data_type) DO UPDATE SET
				version = excluded.version, entities = excluded.entities,
				tombstones = excluded.tombstones, bytes = excluded.bytes`,
			t.chain.ID, dataType, state.Version, state.Entities, state.Tombstones, state.Bytes)
	}
	c, start := t.chain, t.start
	if c.Version != start.Version || c.LiveEntities != start.LiveEntities || c.Bytes != start.Bytes ||
		c.CustomPassphrase != start.CustomPassphrase {
		t.writes.Queue(`
			UPDATE chains SET version = $2, live_entities = $3, bytes_used = $4, custom_passphrase = $5,
				updated_at = now(), last_seen_at = now()
			WHERE id = $1`,
			c.ID, c.Version, c.LiveEntities, c.Bytes, c.CustomPassphrase)
	}
	if t.writes.Len() == 0 {
		return nil
	}
	if err := t.tx.SendBatch(ctx, &t.writes).Close(); err != nil {
		return fmt.Errorf("write chain: %w", err)
	}
	return nil
}
