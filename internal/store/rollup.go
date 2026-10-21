package store

import (
	"context"
	"fmt"
	"time"
)

// How long history is kept.
const (
	MinutesKeptFor = 30 * 24 * time.Hour
	DaysKeptFor    = 380 * 24 * time.Hour
)

// pruneBatch is how many rows one delete removes, so no delete holds a table
// for long.
const pruneBatch = 5000

// Rollup recounts the daily rows for day (a UTC day) from its minutes. Run
// again, it replaces them, so today's rows can be refreshed as it goes.
func (s *Store) Rollup(ctx context.Context, day time.Time) error {
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)

	_, err := s.pool.Exec(ctx, `
		WITH counts AS (
			SELECT project,
			       count(*) FILTER (WHERE state = 1) AS up,
			       count(*) FILTER (WHERE state = 2) AS degraded,
			       count(*) FILTER (WHERE state = 3) AS down
			  FROM sonar.project_minutes
			 WHERE minute >= $2 AND minute < $3 AND state > 0
			 GROUP BY project
		), latency AS (
			SELECT s.project,
			       (sum(c.latency_ms_avg::bigint * (c.attempts - c.failures)) / nullif(sum(c.attempts - c.failures), 0))::integer AS ms
			  FROM sonar.check_minutes c JOIN sonar.services s ON s.id = c.service_id
			 WHERE c.minute >= $2 AND c.minute < $3 AND c.latency_ms_avg IS NOT NULL
			 GROUP BY s.project
		)
		INSERT INTO sonar.project_days (project, day, minutes_up, minutes_degraded, minutes_down, latency_ms_avg)
		SELECT c.project, $1, c.up, c.degraded, c.down, l.ms
		  FROM counts c LEFT JOIN latency l USING (project)
		ON CONFLICT (project, day) DO UPDATE SET
			minutes_up = EXCLUDED.minutes_up, minutes_degraded = EXCLUDED.minutes_degraded,
			minutes_down = EXCLUDED.minutes_down, latency_ms_avg = EXCLUDED.latency_ms_avg`,
		from, from, to)
	if err != nil {
		return fmt.Errorf("database: daily project rows for %s: %w", from.Format(time.DateOnly), err)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO sonar.service_days (service_id, day, minutes_up, minutes_degraded, minutes_down)
		SELECT service_id, $1,
		       count(*) FILTER (WHERE state = 1), count(*) FILTER (WHERE state = 2), count(*) FILTER (WHERE state = 3)
		  FROM sonar.service_minutes
		 WHERE minute >= $2 AND minute < $3 AND counted AND state > 0
		 GROUP BY service_id
		ON CONFLICT (service_id, day) DO UPDATE SET
			minutes_up = EXCLUDED.minutes_up, minutes_degraded = EXCLUDED.minutes_degraded, minutes_down = EXCLUDED.minutes_down`,
		from, from, to)
	if err != nil {
		return fmt.Errorf("database: daily service rows for %s: %w", from.Format(time.DateOnly), err)
	}
	return nil
}

// Prune deletes rows past their retention, a batch at a time, and returns
// how many went.
func (s *Store) Prune(ctx context.Context, now time.Time) (int64, error) {
	minuteCutoff := now.UTC().Add(-MinutesKeptFor)
	dayCutoff := now.UTC().Add(-DaysKeptFor)
	targets := []struct {
		table, column string
		cutoff        time.Time
	}{
		{"host_minutes", "minute", minuteCutoff},
		{"service_minutes", "minute", minuteCutoff},
		{"check_minutes", "minute", minuteCutoff},
		{"project_minutes", "minute", minuteCutoff},
		{"project_days", "day", dayCutoff},
		{"service_days", "day", dayCutoff},
		{"containers", "last_seen", dayCutoff},
	}

	var total int64
	for _, t := range targets {
		query := fmt.Sprintf(`DELETE FROM sonar.%[1]s WHERE ctid IN (SELECT ctid FROM sonar.%[1]s WHERE %[2]s < $1 LIMIT %[3]d)`,
			t.table, t.column, pruneBatch)
		for {
			tag, err := s.pool.Exec(ctx, query, t.cutoff)
			if err != nil {
				return total, fmt.Errorf("database: deleting old rows from %s: %w", t.table, err)
			}
			total += tag.RowsAffected()
			if tag.RowsAffected() < pruneBatch {
				break
			}
		}
	}
	return total, nil
}
