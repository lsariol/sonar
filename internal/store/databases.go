package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DatabaseUse is sparkdb and each database on it, read as sonar_monitor.
type DatabaseUse struct {
	MaxConnections int // the server's max_connections
	Connections    int // client connections to every database
	Databases      []DatabaseRow
}

// DatabaseRow is one database: its size and its client connections.
type DatabaseRow struct {
	Name           string
	SizeBytes      int64
	Connections    int
	Active         int    // running a query right now
	OldestXactSecs *int64 // the oldest open transaction's age; nil without one
}

// DatabaseUse reads every database's size and connections.
func (s *Store) DatabaseUse(ctx context.Context) (DatabaseUse, error) {
	var use DatabaseUse
	err := s.pool.QueryRow(ctx, `
		SELECT current_setting('max_connections')::integer,
		       (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend')`).
		Scan(&use.MaxConnections, &use.Connections)
	if err != nil {
		return use, fmt.Errorf("database: reading sparkdb's connections as SONAR_DATABASE_MONITOR_URL: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT d.datname, pg_database_size(d.oid),
		       count(a.pid) FILTER (WHERE a.backend_type = 'client backend'),
		       count(a.pid) FILTER (WHERE a.backend_type = 'client backend' AND a.state = 'active'),
		       extract(epoch FROM now() - min(a.xact_start) FILTER (WHERE a.backend_type = 'client backend'))::bigint
		  FROM pg_database d LEFT JOIN pg_stat_activity a ON a.datid = d.oid
		 WHERE NOT d.datistemplate AND d.datallowconn
		 GROUP BY d.oid, d.datname
		 ORDER BY d.datname`)
	if err != nil {
		return use, fmt.Errorf("database: reading database sizes as SONAR_DATABASE_MONITOR_URL: %w (the role needs pg_read_all_stats)", err)
	}
	use.Databases, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (DatabaseRow, error) {
		var d DatabaseRow
		err := r.Scan(&d.Name, &d.SizeBytes, &d.Connections, &d.Active, &d.OldestXactSecs)
		return d, err
	})
	if err != nil {
		return use, fmt.Errorf("database: reading database sizes as SONAR_DATABASE_MONITOR_URL: %w (the role needs pg_read_all_stats)", err)
	}
	return use, nil
}

// DatabaseBucket is one database over one stretch of a chart.
type DatabaseBucket struct {
	Database           string
	Start              time.Time
	Minutes            int
	ConnAvg            float64
	ConnMax, ActiveMax int
	SizeBytes          int64 // the last reading in the bucket
}

// DatabaseHistory adds each database's minutes into buckets of the given size, aligned to UTC.
func (s *Store) DatabaseHistory(ctx context.Context, from, to time.Time, bucket time.Duration) ([]DatabaseBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT database, date_bin($3, minute, TIMESTAMPTZ '2000-01-01 00:00:00+00'), count(*),
		       avg(connections)::float8, max(connections), max(active),
		       (array_agg(size_bytes ORDER BY minute DESC))[1]
		  FROM sonar.database_minutes
		 WHERE minute >= $1 AND minute < $2
		 GROUP BY 1, 2
		 ORDER BY 1, 2`, from.UTC(), to.UTC(), bucket)
	if err != nil {
		return nil, fmt.Errorf("database: reading database history: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DatabaseBucket, error) {
		var b DatabaseBucket
		err := r.Scan(&b.Database, &b.Start, &b.Minutes, &b.ConnAvg, &b.ConnMax, &b.ActiveMax, &b.SizeBytes)
		b.Start = b.Start.UTC()
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading database history: %w", err)
	}
	return out, nil
}
