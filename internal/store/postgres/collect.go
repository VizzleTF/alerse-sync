package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

const (
	// collectChains is how many chains Collect works on per round.
	collectChains = 100
	// collectLock keeps server instances from collecting at the same time.
	collectLock = migrationLock + 1
)

// Collect removes what r says is due. Removing entities takes the chain's
// lock, one chain at a time, so it never races a commit. While another
// server instance collects, it does nothing.
func (s *Store) Collect(ctx context.Context, r store.Retention) (store.Collected, error) {
	var done store.Collected
	batch := max(r.Batch, 1)

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return done, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, collectLock).Scan(&locked); err != nil {
		return done, fmt.Errorf("lock collection: %w", err)
	}
	if !locked {
		return done, nil
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, collectLock) //nolint:errcheck // the lock ends with the session anyway

	if r.EmptyChainTTL > 0 {
		// The constant in the condition matches the partial index.
		done.EmptyChains, err = s.deleteChains(ctx, fmt.Sprintf(
			`version <= %d AND created_at < $1`, r.EmptyVersion), r.Now.Add(-r.EmptyChainTTL), batch)
		if err != nil {
			return done, fmt.Errorf("remove empty chains: %w", err)
		}
	}
	if r.InactiveChainTTL > 0 {
		done.InactiveChains, err = s.deleteChains(ctx,
			`last_seen_at < $1`, r.Now.Add(-r.InactiveChainTTL), batch)
		if err != nil {
			return done, fmt.Errorf("remove inactive chains: %w", err)
		}
	}
	if r.ExpiringTypesTTL > 0 && len(r.ExpiringTypes) > 0 {
		types := make([]string, len(r.ExpiringTypes))
		for i, dataType := range r.ExpiringTypes {
			types[i] = strconv.Itoa(int(dataType))
		}
		// Listing the types matches the partial index.
		condition := `data_type IN (` + strings.Join(types, ", ") + `) AND updated_at < $2`
		done.Expired, err = s.deleteEntities(ctx, condition, r.Now.Add(-r.ExpiringTypesTTL), batch, false)
		if err != nil {
			return done, fmt.Errorf("remove expired entities: %w", err)
		}
	}
	if r.TombstoneTTL > 0 {
		done.Tombstones, err = s.deleteEntities(ctx,
			`deleted AND updated_at < $2`, r.Now.Add(-r.TombstoneTTL), batch, true)
		if err != nil {
			return done, fmt.Errorf("remove tombstones: %w", err)
		}
	}
	if r.RemovedDeviceTTL > 0 {
		done.RemovedDevices, err = s.collectDevices(ctx, r.Now.Add(-r.RemovedDeviceTTL))
		if err != nil {
			return done, fmt.Errorf("remove device records: %w", err)
		}
	}
	return done, nil
}

// deleteChains removes the chains matching condition, which takes the
// cutoff as $1, batch by batch.
func (s *Store) deleteChains(ctx context.Context, condition string, cutoff time.Time, batch int) (int64, error) {
	var total int64
	for ctx.Err() == nil {
		// The condition repeats outside the subquery so that a chain that
		// changed while the statement waited for its lock stays.
		tag, err := s.pool.Exec(ctx, `
			DELETE FROM chains WHERE id IN (SELECT id FROM chains WHERE `+condition+` LIMIT $2)
			AND `+condition, cutoff, batch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			break
		}
	}
	return total, ctx.Err()
}

// deleteEntities removes the entities matching condition, which takes the
// chain as $1 and the cutoff as $2. purge raises PurgedVersion.
func (s *Store) deleteEntities(ctx context.Context, condition string, cutoff time.Time, batch int, purge bool) (int64, error) {
	var total int64
	for ctx.Err() == nil {
		rows, err := s.pool.Query(ctx, `SELECT DISTINCT chain_id FROM entities WHERE `+
			strings.ReplaceAll(condition, "$2", "$1")+` LIMIT $2`, cutoff, collectChains)
		if err != nil {
			return total, err
		}
		chains, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return total, err
		}
		for _, chainID := range chains {
			n, err := s.deleteChainEntities(ctx, chainID, condition, cutoff, batch, purge)
			total += n
			if err != nil {
				return total, err
			}
		}
		if len(chains) < collectChains {
			break
		}
	}
	return total, ctx.Err()
}

func (s *Store) deleteChainEntities(ctx context.Context, chainID, condition string, cutoff time.Time,
	batch int, purge bool,
) (int64, error) {
	var total int64
	for ctx.Err() == nil {
		var removed int64
		err := pgx.BeginFunc(ctx, s.pool, func(pgTx pgx.Tx) error {
			chain, ok, err := readChain(ctx, pgTx, chainID, true)
			if err != nil || !ok {
				return err
			}
			rows, err := pgTx.Query(ctx, `
				DELETE FROM entities WHERE chain_id = $1 AND id IN (
					SELECT id FROM entities WHERE chain_id = $1 AND `+condition+` LIMIT $3)
				RETURNING data_type, version, deleted, octet_length(data)`,
				chainID, cutoff, batch)
			if err != nil {
				return err
			}
			start := chain.Clone()
			for rows.Next() {
				var dataType int32
				var version, length int64
				var deleted bool
				if err := rows.Scan(&dataType, &version, &deleted, &length); err != nil {
					rows.Close()
					return err
				}
				chain.Remove(dataType, deleted, length)
				if purge {
					state := chain.Types[dataType]
					state.PurgedVersion = max(state.PurgedVersion, version)
					chain.Types[dataType] = state
				}
				removed++
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if removed == 0 {
				return nil
			}
			var writes pgx.Batch
			for dataType, state := range chain.Types {
				if state == start.Types[dataType] {
					continue
				}
				writes.Queue(`
					UPDATE chain_types SET purged_version = $3, entities = $4, tombstones = $5, bytes = $6
					WHERE chain_id = $1 AND data_type = $2`,
					chainID, dataType, state.PurgedVersion, state.Entities, state.Tombstones, state.Bytes)
			}
			writes.Queue(`UPDATE chains SET live_entities = $2, bytes_used = $3 WHERE id = $1`,
				chainID, chain.LiveEntities, chain.Bytes)
			return pgTx.SendBatch(ctx, &writes).Close()
		})
		total += removed
		if err != nil {
			return total, err
		}
		if removed < int64(batch) {
			break
		}
	}
	return total, ctx.Err()
}
