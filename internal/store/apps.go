package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AppCounterDay is one counter's total for one UTC day.
type AppCounterDay struct {
	Project, Name string
	Day           time.Time
	Total         int64
}

// AppCounterDays returns every counter's daily totals between two UTC dates, both included.
func (s *Store) AppCounterDays(ctx context.Context, from, to time.Time) ([]AppCounterDay, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT project, name, day, total
		  FROM sonar.app_counter_days
		 WHERE day >= $1 AND day <= $2
		 ORDER BY project, name, day`, dateOnly(from), dateOnly(to))
	if err != nil {
		return nil, fmt.Errorf("database: reading app counters: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AppCounterDay, error) {
		var d AppCounterDay
		err := r.Scan(&d.Project, &d.Name, &d.Day, &d.Total)
		d.Day = d.Day.UTC()
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading app counters: %w", err)
	}
	return out, nil
}

// AppCheckChanges returns the check changes since since, newest first.
func (s *Store) AppCheckChanges(ctx context.Context, since time.Time) ([]AppCheckChange, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT project, service, name, at, ok, message
		  FROM sonar.app_check_changes
		 WHERE at >= $1
		 ORDER BY at DESC
		 LIMIT 1000`, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("database: reading app check changes: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AppCheckChange, error) {
		var c AppCheckChange
		err := r.Scan(&c.Project, &c.Service, &c.Name, &c.At, &c.OK, &c.Message)
		c.At = c.At.UTC()
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading app check changes: %w", err)
	}
	return out, nil
}
