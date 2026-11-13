//go:build integration

package account

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/VizzleTF/arcomium-sync/internal/store/postgres"
	"github.com/VizzleTF/arcomium-sync/internal/testpg"
)

// TestMain runs the account tests on Postgres.
func TestMain(m *testing.M) {
	ctx := context.Background()
	pool, stop, err := testpg.Start(ctx)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	newStore = func(t *testing.T) testStore {
		t.Helper()
		for _, table := range []string{"accounts", "chains"} {
			if _, err := pool.Exec(ctx, `DELETE FROM `+table); err != nil {
				t.Fatal(err)
			}
		}
		return postgres.New(pool)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}
