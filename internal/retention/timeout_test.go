package retention

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

type failingStore struct {
	store.Store
	err error
}

func (s failingStore) Collect(context.Context, store.Retention) (store.Collected, error) {
	return store.Collected{}, s.err
}

func TestOnceLogsARunThatTimesOut(t *testing.T) {
	var out bytes.Buffer
	job := Job{Store: failingStore{err: context.DeadlineExceeded}, Log: slog.New(slog.NewTextHandler(&out, nil))}
	job.Once(context.Background())
	if !strings.Contains(out.String(), "retention job failed") {
		t.Errorf("a failed run logged %q", out.String())
	}
}
