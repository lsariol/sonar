package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DiskPoint is one project's disk use at the last measurement of a stretch
// of a chart. Project "" is the totals.
type DiskPoint struct {
	Project                                 string
	Start, MeasuredAt                       time.Time
	DataBytes                               *int64
	DataUnreadable                          int64
	ImageBytes, ContainerBytes, VolumeBytes int64
	BuildCacheBytes                         int64 // totals only
}

// DiskHistory returns, for each project and for the totals, the last
// measurement in each bucket of the given size from from to to, aligned to
// UTC; the totals come first.
func (s *Store) DiskHistory(ctx context.Context, from, to time.Time, bucket time.Duration) ([]DiskPoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (b) '', date_bin($3, measured_at, TIMESTAMPTZ '2000-01-01 00:00:00+00') AS b, measured_at,
		       data_bytes, 0, image_bytes, container_bytes, volume_bytes, build_cache_bytes
		  FROM sonar.disk_totals
		 WHERE measured_at >= $1 AND measured_at < $2
		 ORDER BY b, measured_at DESC`, from.UTC(), to.UTC(), bucket)
	if err != nil {
		return nil, fmt.Errorf("database: reading disk history: %w", err)
	}
	totals, err := pgx.CollectRows(rows, scanDiskPoint)
	if err != nil {
		return nil, fmt.Errorf("database: reading disk history: %w", err)
	}
	rows, err = s.pool.Query(ctx, `
		SELECT DISTINCT ON (project, b) project, date_bin($3, measured_at, TIMESTAMPTZ '2000-01-01 00:00:00+00') AS b, measured_at,
		       data_bytes, data_unreadable, image_bytes, container_bytes, volume_bytes, 0
		  FROM sonar.project_disk
		 WHERE measured_at >= $1 AND measured_at < $2
		 ORDER BY project, b, measured_at DESC`, from.UTC(), to.UTC(), bucket)
	if err != nil {
		return nil, fmt.Errorf("database: reading disk history: %w", err)
	}
	projects, err := pgx.CollectRows(rows, scanDiskPoint)
	if err != nil {
		return nil, fmt.Errorf("database: reading disk history: %w", err)
	}
	return append(totals, projects...), nil
}

func scanDiskPoint(r pgx.CollectableRow) (DiskPoint, error) {
	var p DiskPoint
	err := r.Scan(&p.Project, &p.Start, &p.MeasuredAt, &p.DataBytes, &p.DataUnreadable, &p.ImageBytes, &p.ContainerBytes,
		&p.VolumeBytes, &p.BuildCacheBytes)
	p.Start, p.MeasuredAt = p.Start.UTC(), p.MeasuredAt.UTC()
	return p, err
}
