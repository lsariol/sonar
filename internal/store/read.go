package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DayCounts is one project's minute counts for one UTC day.
type DayCounts struct {
	Project            string
	Day                time.Time
	Up, Degraded, Down int
	LatencyMs          *int
}

// ProjectDays returns the daily rows from one UTC date to another, both
// included.
func (s *Store) ProjectDays(ctx context.Context, from, to time.Time) ([]DayCounts, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT project, day, minutes_up, minutes_degraded, minutes_down, latency_ms_avg
		  FROM sonar.project_days
		 WHERE day >= $1 AND day <= $2
		 ORDER BY project, day`, dateOnly(from), dateOnly(to))
	if err != nil {
		return nil, fmt.Errorf("database: reading daily rows: %w", err)
	}
	days, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DayCounts, error) {
		var d DayCounts
		err := r.Scan(&d.Project, &d.Day, &d.Up, &d.Degraded, &d.Down, &d.LatencyMs)
		d.Day = d.Day.UTC()
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading daily rows: %w", err)
	}
	return days, nil
}

// ResponseTimes is each project's average latency of answered checks since
// since, in milliseconds. Projects without checks aren't in it.
func (s *Store) ResponseTimes(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.project,
		       (sum(c.latency_ms_avg::bigint * (c.attempts - c.failures)) / nullif(sum(c.attempts - c.failures), 0))::integer
		  FROM sonar.check_minutes c JOIN sonar.services s ON s.id = c.service_id
		 WHERE c.minute >= $1 AND c.latency_ms_avg IS NOT NULL
		 GROUP BY s.project`, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("database: reading response times: %w", err)
	}
	out := make(map[string]int)
	var project string
	var ms *int
	_, err = pgx.ForEachRow(rows, []any{&project, &ms}, func() error {
		if ms != nil {
			out[project] = *ms
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading response times: %w", err)
	}
	return out, nil
}

// HostBucket is the host over one stretch of the graph.
type HostBucket struct {
	Start                        time.Time
	Samples, Expected            int
	CPUAvg, CPUMin, CPUMax       *float64 // nil without a CPU reading
	MemPctAvg, MemPctMin, MemMax *float64
}

// HostHistory adds the host's minutes from from to to into buckets of the
// given size, aligned to UTC. Buckets without minutes aren't returned.
func (s *Store) HostHistory(ctx context.Context, from, to time.Time, bucket time.Duration) ([]HostBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT date_bin($3, minute, TIMESTAMPTZ '2000-01-01 00:00:00+00') AS bucket,
		       sum(samples), sum(expected),
		       sum(cpu_pct_avg * cpu_samples) / nullif(sum(cpu_samples) FILTER (WHERE cpu_pct_avg IS NOT NULL), 0),
		       min(cpu_pct_min), max(cpu_pct_max),
		       sum(mem_used_pct_avg * samples) / nullif(sum(samples), 0),
		       min(mem_used_pct_min), max(mem_used_pct_max)
		  FROM sonar.host_minutes
		 WHERE minute >= $1 AND minute < $2
		 GROUP BY 1
		 ORDER BY 1`, from.UTC(), to.UTC(), bucket)
	if err != nil {
		return nil, fmt.Errorf("database: reading host history: %w", err)
	}
	buckets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (HostBucket, error) {
		var b HostBucket
		var cpuAvg, cpuMin, cpuMax, memAvg, memMin, memMax *float64
		err := r.Scan(&b.Start, &b.Samples, &b.Expected, &cpuAvg, &cpuMin, &cpuMax, &memAvg, &memMin, &memMax)
		b.Start = b.Start.UTC()
		b.CPUAvg, b.CPUMin, b.CPUMax = cpuAvg, cpuMin, cpuMax
		b.MemPctAvg, b.MemPctMin, b.MemMax = memAvg, memMin, memMax
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading host history: %w", err)
	}
	return buckets, nil
}

func dateOnly(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
