//go:build integration

// Package testpg starts a throwaway PostgreSQL for tests.
package testpg

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VizzleTF/arcomium-sync/internal/store/postgres"
)

// Start starts Postgres with the server's schema. stop ends it.
func Start(ctx context.Context) (pool *pgxpool.Pool, stop func(), err error) {
	port, err := freePort()
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "arcomium-sync-pg")
	if err != nil {
		return nil, nil, err
	}
	db := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)).
		RuntimePath(filepath.Join(dir, "runtime")).
		DataPath(filepath.Join(dir, "data")).
		Logger(io.Discard))
	if err := db.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("start postgres: %w", err)
	}
	stop = func() {
		if pool != nil {
			pool.Close()
		}
		_ = db.Stop()
		_ = os.RemoveAll(dir)
	}
	pool, err = pgxpool.New(ctx, fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres", port))
	if err != nil {
		stop()
		return nil, nil, err
	}
	// Running the migrations twice must be harmless: every start runs them.
	for range 2 {
		if err := postgres.New(pool).Migrate(ctx); err != nil {
			stop()
			return nil, nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return pool, stop, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
