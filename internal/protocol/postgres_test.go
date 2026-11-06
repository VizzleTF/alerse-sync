//go:build integration

package protocol

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	"github.com/VizzleTF/arcomium-sync/internal/store/postgres"
	"github.com/VizzleTF/arcomium-sync/internal/testpg"
)

// TestMain runs the protocol tests on Postgres.
func TestMain(m *testing.M) {
	ctx := context.Background()
	pool, stop, err := testpg.Start(ctx)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	newStore = func(t *testing.T) store.Store {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM chains`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { checkCounts(t, pool) })
		return postgres.New(pool)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

// checkCounts fails t if the counts and sizes the store keeps differ from
// what the entities add up to.
func checkCounts(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var types, chains, total int64
	err := pool.QueryRow(context.Background(), `
		WITH actual AS (
			SELECT chain_id, data_type, max(version) AS version,
				count(*) FILTER (WHERE NOT deleted) AS entities, count(*) FILTER (WHERE deleted) AS tombstones,
				sum(octet_length(data)) AS bytes
			FROM entities GROUP BY chain_id, data_type)
		SELECT
			(SELECT count(*) FROM chain_types t FULL JOIN actual a USING (chain_id, data_type)
				WHERE coalesce(t.entities, 0) <> coalesce(a.entities, 0)
				OR coalesce(t.tombstones, 0) <> coalesce(a.tombstones, 0)
				OR coalesce(t.bytes, 0) <> coalesce(a.bytes, 0)
				OR coalesce(t.version, 0) < coalesce(a.version, 0)),
			(SELECT count(*) FROM chains c WHERE
				live_entities <> (SELECT coalesce(sum(entities), 0) FROM actual a WHERE a.chain_id = c.id)
				OR bytes_used <> (SELECT coalesce(sum(bytes), 0) FROM actual a WHERE a.chain_id = c.id)),
			(SELECT n - (SELECT count(*) FROM chains) FROM chain_count)`,
	).Scan(&types, &chains, &total)
	if err != nil {
		t.Fatal(err)
	}
	if types != 0 || chains != 0 || total != 0 {
		t.Errorf("stored counts are off: %d data types, %d chains, chain count by %d", types, chains, total)
	}
}
