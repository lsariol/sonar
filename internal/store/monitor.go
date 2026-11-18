package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lsariol/sonar/internal/state"
)

// StateRun is where a project's current state began, as far as the history
// goes back.
type StateRun struct {
	LastOther *time.Time // the last minute with another known state; nil when none is kept
	First     *time.Time // the project's first kept minute; nil without history
}

// StateRuns returns, for each project and its current state, the last minute
// it had another state and its first kept minute.
func (s *Store) StateRuns(ctx context.Context, current map[string]state.State) (map[string]StateRun, error) {
	projects := make([]string, 0, len(current))
	states := make([]int16, 0, len(current))
	for p, st := range current {
		projects = append(projects, p)
		states = append(states, int16(st))
	}
	rows, err := s.pool.Query(ctx, `
		SELECT p.project,
		       (SELECT m.minute FROM sonar.project_minutes m
		         WHERE m.project = p.project AND m.state <> p.state ORDER BY m.minute DESC LIMIT 1),
		       (SELECT min(m.minute) FROM sonar.project_minutes m WHERE m.project = p.project)
		  FROM unnest($1::text[], $2::smallint[]) AS p(project, state)`, projects, states)
	if err != nil {
		return nil, fmt.Errorf("database: reading state runs: %w", err)
	}
	out := make(map[string]StateRun, len(current))
	var project string
	var other, first *time.Time
	_, err = pgx.ForEachRow(rows, []any{&project, &other, &first}, func() error {
		out[project] = StateRun{LastOther: utcPtr(other), First: utcPtr(first)}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading state runs: %w", err)
	}
	return out, nil
}

// Span is a run of minutes in which a project had one state, with no
// minute missing.
type Span struct {
	Project    string
	State      state.State
	Start, End time.Time // End is the end of the last minute
	Minutes    int
}

// StateSpans returns every project's runs of one state from from to to,
// by project, oldest first.
func (s *Store) StateSpans(ctx context.Context, from, to time.Time) ([]Span, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT project, state, min(minute), max(minute) + interval '1 minute', count(*)
		  FROM (SELECT project, minute, state,
		               minute - row_number() OVER (PARTITION BY project, state ORDER BY minute) * interval '1 minute' AS run
		          FROM sonar.project_minutes
		         WHERE minute >= $1 AND minute < $2) m
		 GROUP BY project, state, run
		 ORDER BY project, 3`, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("database: reading state spans: %w", err)
	}
	spans, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Span, error) {
		var sp Span
		var st int16
		err := r.Scan(&sp.Project, &st, &sp.Start, &sp.End, &sp.Minutes)
		sp.State, sp.Start, sp.End = state.State(st), sp.Start.UTC(), sp.End.UTC()
		return sp, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading state spans: %w", err)
	}
	return spans, nil
}

// ServiceTrouble is a minute in which a counted service wasn't up.
type ServiceTrouble struct {
	Project, Service string
	Minute           time.Time
	State            state.State
}

// ServiceTroubles returns the minutes from from to to in which a counted
// service was degraded, down or updating.
func (s *Store) ServiceTroubles(ctx context.Context, from, to time.Time) ([]ServiceTrouble, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.project, s.service, m.minute, m.state
		  FROM sonar.service_minutes m JOIN sonar.services s ON s.id = m.service_id
		 WHERE m.minute >= $1 AND m.minute < $2 AND m.counted AND m.state IN (2, 3, 4)
		 ORDER BY m.minute`, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("database: reading service trouble: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ServiceTrouble, error) {
		var t ServiceTrouble
		var st int16
		err := r.Scan(&t.Project, &t.Service, &t.Minute, &st)
		t.State, t.Minute = state.State(st), t.Minute.UTC()
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading service trouble: %w", err)
	}
	return out, nil
}

// CheckTrouble is a minute in which an outside check failed or was slow.
type CheckTrouble struct {
	Project, Service, URL string
	Minute                time.Time
	State                 state.State
	Failures, Slow        int
	LastError             string
}

// CheckTroubles returns the minutes from from to to in which an outside
// check failed, was slow, or wasn't up.
func (s *Store) CheckTroubles(ctx context.Context, from, to time.Time) ([]CheckTrouble, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.project, s.service, c.url, c.minute, c.state, c.failures, c.slow, coalesce(c.last_error, '')
		  FROM sonar.check_minutes c JOIN sonar.services s ON s.id = c.service_id
		 WHERE c.minute >= $1 AND c.minute < $2 AND (c.failures > 0 OR c.slow > 0 OR c.state IN (2, 3))
		 ORDER BY c.minute`, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("database: reading check trouble: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CheckTrouble, error) {
		var t CheckTrouble
		var st int16
		err := r.Scan(&t.Project, &t.Service, &t.URL, &t.Minute, &st, &t.Failures, &t.Slow, &t.LastError)
		t.State, t.Minute = state.State(st), t.Minute.UTC()
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading check trouble: %w", err)
	}
	return out, nil
}

// UsageBucket is one service's use over one stretch of a chart.
type UsageBucket struct {
	Project, Service  string
	Start             time.Time
	Minutes           int
	CPUAvg, CPUMax    *float64 // cores; nil without a reading
	MemAvg, MemMax    *int64   // bytes
	NetRx, NetTx      int64    // bytes moved
	BlkRead, BlkWrite int64
}

// ServiceUsage adds each service's minutes from from to to into buckets of
// the given size, aligned to UTC; project "" means every project. Buckets
// without minutes aren't returned.
func (s *Store) ServiceUsage(ctx context.Context, from, to time.Time, bucket time.Duration, project string) ([]UsageBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.project, s.service, date_bin($3, m.minute, TIMESTAMPTZ '2000-01-01 00:00:00+00'), count(*),
		       avg(m.cpu_cores_avg), max(m.cpu_cores_max),
		       avg(m.mem_bytes_avg)::bigint, max(m.mem_bytes_max),
		       sum(m.net_rx_bytes)::bigint, sum(m.net_tx_bytes)::bigint, sum(m.blk_read_bytes)::bigint, sum(m.blk_write_bytes)::bigint
		  FROM sonar.service_minutes m JOIN sonar.services s ON s.id = m.service_id
		 WHERE m.minute >= $1 AND m.minute < $2 AND ($4::text = '' OR s.project = $4::text)
		 GROUP BY 1, 2, 3
		 ORDER BY 1, 2, 3`, from.UTC(), to.UTC(), bucket, project)
	if err != nil {
		return nil, fmt.Errorf("database: reading service usage: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (UsageBucket, error) {
		var b UsageBucket
		err := r.Scan(&b.Project, &b.Service, &b.Start, &b.Minutes, &b.CPUAvg, &b.CPUMax, &b.MemAvg, &b.MemMax,
			&b.NetRx, &b.NetTx, &b.BlkRead, &b.BlkWrite)
		b.Start = b.Start.UTC()
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading service usage: %w", err)
	}
	return out, nil
}

// HostDetailBucket is the host over one stretch of a chart, with everything
// the minutes keep.
type HostDetailBucket struct {
	HostBucket
	MemUsedAvg, MemTotal *int64
	SwapUsedAvg          *int64
	Load1Avg, Load1Max   *float64
	DiskUsed, DiskTotal  *int64 // the last reading in the bucket
}

// HostDetail is HostHistory with memory in bytes, swap, load and the disk.
func (s *Store) HostDetail(ctx context.Context, from, to time.Time, bucket time.Duration) ([]HostDetailBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT date_bin($3, minute, TIMESTAMPTZ '2000-01-01 00:00:00+00') AS bucket,
		       sum(samples), sum(expected),
		       sum(cpu_pct_avg * cpu_samples) / nullif(sum(cpu_samples) FILTER (WHERE cpu_pct_avg IS NOT NULL), 0),
		       min(cpu_pct_min), max(cpu_pct_max),
		       sum(mem_used_pct_avg * samples) / nullif(sum(samples), 0),
		       min(mem_used_pct_min), max(mem_used_pct_max),
		       (sum(mem_used_bytes_avg * samples) / nullif(sum(samples), 0))::bigint, max(mem_total_bytes),
		       (sum(swap_used_bytes_avg * samples) / nullif(sum(samples), 0))::bigint,
		       sum(load1_avg * samples) / nullif(sum(samples), 0), max(load1_max),
		       (array_agg(disk_used_bytes ORDER BY minute DESC) FILTER (WHERE disk_used_bytes IS NOT NULL))[1],
		       (array_agg(disk_total_bytes ORDER BY minute DESC) FILTER (WHERE disk_total_bytes IS NOT NULL))[1]
		  FROM sonar.host_minutes
		 WHERE minute >= $1 AND minute < $2
		 GROUP BY 1
		 ORDER BY 1`, from.UTC(), to.UTC(), bucket)
	if err != nil {
		return nil, fmt.Errorf("database: reading host detail: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (HostDetailBucket, error) {
		var b HostDetailBucket
		err := r.Scan(&b.Start, &b.Samples, &b.Expected, &b.CPUAvg, &b.CPUMin, &b.CPUMax, &b.MemPctAvg, &b.MemPctMin, &b.MemMax,
			&b.MemUsedAvg, &b.MemTotal, &b.SwapUsedAvg, &b.Load1Avg, &b.Load1Max, &b.DiskUsed, &b.DiskTotal)
		b.Start = b.Start.UTC()
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading host detail: %w", err)
	}
	return out, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
