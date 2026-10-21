package agent

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/lsariol/sonar/internal/store"
)

const (
	databaseCollector = "database"

	// flushDelay is how long after a minute ends it's written, so the last
	// readings of the minute are in.
	flushDelay   = 5 * time.Second
	flushEvery   = 5 * time.Second
	maxPending   = 60 // finished minutes kept while the database is unreachable
	rollupEvery  = 5 * time.Minute
	pruneEvery   = time.Hour
	connectRetry = 15 * time.Second
	dbTimeout    = 30 * time.Second
)

// Store is where the history goes: sonar_db, as sonar_app.
type Store interface {
	WriteMinute(ctx context.Context, m store.Minute) error
	Rollup(ctx context.Context, day time.Time) error
	Prune(ctx context.Context, now time.Time) (int64, error)
}

// runDatabase connects (retrying while sparkdb is away), then writes finished
// minutes, refreshes the daily rows and deletes old rows until ctx ends.
// Collecting goes on meanwhile; minutes wait in the queue.
func (a *Agent) runDatabase(ctx context.Context) {
	var db Store
	for db == nil {
		s, err := a.opts.OpenStore(ctx)
		a.mu.Lock()
		a.record(databaseCollector, err)
		a.mu.Unlock()
		if err == nil {
			db = s
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(connectRetry):
		}
	}

	t := time.NewTicker(flushEvery)
	defer t.Stop()
	var lastRollup, lastPrune time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.flush(ctx, db)

		now := a.opts.Now().UTC()
		if now.Sub(lastRollup) >= rollupEvery {
			lastRollup = now
			for _, day := range []time.Time{now.AddDate(0, 0, -1), now} {
				rctx, cancel := context.WithTimeout(ctx, dbTimeout)
				if err := db.Rollup(rctx, day); err != nil {
					a.opts.Log.Warn("refreshing the daily rows failed", "err", err)
				}
				cancel()
			}
		}
		if now.Sub(lastPrune) >= pruneEvery {
			lastPrune = now
			pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			n, err := db.Prune(pctx, now)
			cancel()
			switch {
			case err != nil:
				a.opts.Log.Warn("deleting old rows failed", "err", err)
			case n > 0:
				a.opts.Log.Info("deleted rows past their retention", "rows", n)
			}
		}
	}
}

// flush closes finished minutes and writes the queue in order, stopping at
// the first failure so nothing is written out of order.
func (a *Agent) flush(ctx context.Context, db Store) {
	a.mu.Lock()
	a.closeMinutes(a.opts.Now())
	pending := slices.Clone(a.pending)
	a.mu.Unlock()
	if len(pending) == 0 {
		return
	}

	written := 0
	var err error
	for _, m := range pending {
		wctx, cancel := context.WithTimeout(ctx, dbTimeout)
		err = db.WriteMinute(wctx, m)
		cancel()
		if err != nil {
			break
		}
		written++
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = a.pending[written:]
	if err != nil {
		err = fmt.Errorf("%w (%d %s waiting to be written)", err, len(a.pending), plural(len(a.pending), "minute", "minutes"))
	}
	a.record(databaseCollector, err)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
