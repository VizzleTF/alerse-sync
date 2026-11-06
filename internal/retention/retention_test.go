package retention

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	"github.com/VizzleTF/arcomium-sync/internal/store/memory"
)

func TestRun(t *testing.T) {
	st := memory.New()
	if err := st.Create(context.Background(), "chain", 0, func(store.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Nobody ever committed to the chain.
	done := make(chan store.Collected, 1)
	job := Job{
		Store:    st,
		Policy:   store.Retention{EmptyChainTTL: time.Nanosecond, EmptyVersion: 5},
		Interval: time.Millisecond,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Done: func(c store.Collected) {
			select {
			case done <- c:
			default:
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go job.Run(ctx)
	select {
	case c := <-done:
		if c.EmptyChains != 1 {
			t.Fatalf("collected %+v, want the empty chain", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not run")
	}
}
