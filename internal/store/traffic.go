package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TrafficBucket is one hostname's traffic over one stretch of a chart.
type TrafficBucket struct {
	Host                    string
	Start                   time.Time
	Requests, Errors, Bytes int64
}

// TrafficHistory adds each hostname's hours into buckets of the given size, aligned to UTC.
func (s *Store) TrafficHistory(ctx context.Context, from, to time.Time, bucket time.Duration) ([]TrafficBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT host, date_bin($3, hour, TIMESTAMPTZ '2000-01-01 00:00:00+00'),
		       sum(requests)::bigint, sum(errors_5xx)::bigint, sum(bytes)::bigint
		  FROM sonar.traffic_hours
		 WHERE hour >= $1 AND hour < $2
		 GROUP BY 1, 2
		 ORDER BY 1, 2`, from.UTC(), to.UTC(), bucket)
	if err != nil {
		return nil, fmt.Errorf("database: reading traffic: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TrafficBucket, error) {
		var b TrafficBucket
		err := r.Scan(&b.Host, &b.Start, &b.Requests, &b.Errors, &b.Bytes)
		b.Start = b.Start.UTC()
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading traffic: %w", err)
	}
	return out, nil
}
