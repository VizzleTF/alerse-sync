// Package retention runs the job that removes what the server no longer
// needs to keep: old tombstones, expired history and chains nobody uses.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/store"
)

// firstRun is how long after the start the job first runs, so that a server
// that restarts often does not run it every time it starts.
const firstRun = time.Minute

// Job removes what Policy says is due, every Interval.
type Job struct {
	Store    store.Store
	Policy   store.Retention
	Interval time.Duration
	Log      *slog.Logger
	// Done, if set, is told what each run removed.
	Done func(store.Collected)
}

// Run runs the job until ctx ends.
func (j Job) Run(ctx context.Context) {
	if j.Interval <= 0 {
		return
	}
	timer := time.NewTimer(min(firstRun, j.Interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		j.Once(ctx)
		timer.Reset(j.Interval)
	}
}

// Once runs the job once.
func (j Job) Once(ctx context.Context) {
	policy := j.Policy
	policy.Now = time.Now()
	if policy.Batch <= 0 {
		policy.Batch = 1000
	}
	run, cancel := context.WithTimeout(ctx, max(j.Interval, time.Minute))
	defer cancel()
	start := time.Now()
	done, err := j.Store.Collect(run, policy)
	if j.Done != nil {
		j.Done(done)
	}
	// Quiet only when the server shuts down; a run that takes too long is
	// a failure to know about.
	if err != nil && ctx.Err() == nil {
		j.Log.Error("retention job failed", "error", err)
	}
	if done != (store.Collected{}) {
		j.Log.Info("retention job removed old data", "tombstones", done.Tombstones, "expired", done.Expired,
			"empty_chains", done.EmptyChains, "inactive_chains", done.InactiveChains, "removed_devices", done.RemovedDevices,
			"took", time.Since(start).Round(time.Millisecond))
	}
}
