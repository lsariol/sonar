package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lsariol/sonar/internal/state"
)

// Minute is one finished minute of what the agent saw.
type Minute struct {
	Start      time.Time
	Host       *HostMinute // nil when the host wasn't read that minute
	Services   []ServiceMinute
	Checks     []CheckMinute
	Projects   []ProjectMinute
	Containers []ContainerSeen
	Updates    []Update // updates that ended during the minute
}

// Update is one service being replaced by a deploy.
type Update struct {
	Project, Service string
	Started, Ended   time.Time
	From, To         string // versions (com.mobasity.version)
	Outcome          string // ok, failed or timed out
}

// HostMinute is the host over one minute.
type HostMinute struct {
	Samples, Expected, CPUSamples   int
	CPUAvg, CPUMin, CPUMax          *float64 // nil without a CPU reading
	MemPctAvg, MemPctMin, MemPctMax float64
	MemUsedAvg, MemTotal            uint64
	SwapUsedAvg                     uint64
	Load1Avg, Load1Max              float64
	DiskUsed, DiskTotal             *uint64
}

// ServiceMinute is one compose service over one minute.
type ServiceMinute struct {
	Project, Service  string
	Samples           int
	State             state.State // the worst seen
	Counted           bool
	Containers        int
	CPUAvg, CPUMax    *float64 // cores
	MemAvg, MemMax    *uint64
	NetRx, NetTx      uint64 // bytes moved during the minute
	BlkRead, BlkWrite uint64
}

// CheckMinute is one service's outside checks over one minute.
type CheckMinute struct {
	Project, Service, URL    string
	Attempts, Failures, Slow int
	LatencyAvgMs             *int // over the checks that answered
	LatencyMaxMs             *int
	State                    state.State // after the minute's last check
}

// ProjectMinute is a project's worst known state over one minute.
type ProjectMinute struct {
	Project  string
	State    state.State
	Evidence state.Evidence
}

// ContainerSeen is a container that existed during the minute.
type ContainerSeen struct {
	ID, Project, Service string
	Name, Image, Version string
	CreatedAt            time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
	ExitCode             int
}

// WriteMinute saves m in one transaction. Writing a minute again replaces
// it, so retrying after a failure is safe.
func (s *Store) WriteMinute(ctx context.Context, m Minute) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: writing minute %s: %w", m.Start.Format(time.RFC3339), err)
	}
	defer tx.Rollback(ctx)

	ids, err := upsertServices(ctx, tx, m)
	if err != nil {
		return err
	}
	start, end := m.Start.UTC(), m.Start.UTC().Add(time.Minute)

	b := &pgx.Batch{}
	for _, c := range m.Containers {
		b.Queue(`
			INSERT INTO sonar.containers (id, service_id, name, image, version, created_at, started_at, finished_at, exit_code, first_seen, last_seen)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (id) DO UPDATE SET
				service_id = EXCLUDED.service_id, name = EXCLUDED.name, image = EXCLUDED.image, version = EXCLUDED.version,
				started_at = EXCLUDED.started_at, finished_at = EXCLUDED.finished_at, exit_code = EXCLUDED.exit_code,
				first_seen = LEAST(containers.first_seen, EXCLUDED.first_seen),
				last_seen = GREATEST(containers.last_seen, EXCLUDED.last_seen)`,
			c.ID, ids[key(c.Project, c.Service)], c.Name, c.Image, c.Version, nullTime(c.CreatedAt), c.StartedAt, c.FinishedAt,
			c.ExitCode, start, end)
	}
	if h := m.Host; h != nil {
		b.Queue(`
			INSERT INTO sonar.host_minutes (minute, samples, expected, cpu_samples, cpu_pct_avg, cpu_pct_min, cpu_pct_max,
				mem_used_pct_avg, mem_used_pct_min, mem_used_pct_max, mem_used_bytes_avg, mem_total_bytes, swap_used_bytes_avg,
				load1_avg, load1_max, disk_used_bytes, disk_total_bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			ON CONFLICT (minute) DO UPDATE SET
				samples = EXCLUDED.samples, expected = EXCLUDED.expected, cpu_samples = EXCLUDED.cpu_samples,
				cpu_pct_avg = EXCLUDED.cpu_pct_avg, cpu_pct_min = EXCLUDED.cpu_pct_min, cpu_pct_max = EXCLUDED.cpu_pct_max,
				mem_used_pct_avg = EXCLUDED.mem_used_pct_avg, mem_used_pct_min = EXCLUDED.mem_used_pct_min,
				mem_used_pct_max = EXCLUDED.mem_used_pct_max, mem_used_bytes_avg = EXCLUDED.mem_used_bytes_avg,
				mem_total_bytes = EXCLUDED.mem_total_bytes, swap_used_bytes_avg = EXCLUDED.swap_used_bytes_avg,
				load1_avg = EXCLUDED.load1_avg, load1_max = EXCLUDED.load1_max,
				disk_used_bytes = EXCLUDED.disk_used_bytes, disk_total_bytes = EXCLUDED.disk_total_bytes`,
			start, h.Samples, h.Expected, h.CPUSamples, h.CPUAvg, h.CPUMin, h.CPUMax,
			h.MemPctAvg, h.MemPctMin, h.MemPctMax, int64(h.MemUsedAvg), int64(h.MemTotal), int64(h.SwapUsedAvg),
			h.Load1Avg, h.Load1Max, optInt64(h.DiskUsed), optInt64(h.DiskTotal))
	}
	for _, sm := range m.Services {
		b.Queue(`
			INSERT INTO sonar.service_minutes (service_id, minute, samples, state, counted, containers,
				cpu_cores_avg, cpu_cores_max, mem_bytes_avg, mem_bytes_max, net_rx_bytes, net_tx_bytes, blk_read_bytes, blk_write_bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (service_id, minute) DO UPDATE SET
				samples = EXCLUDED.samples, state = EXCLUDED.state, counted = EXCLUDED.counted, containers = EXCLUDED.containers,
				cpu_cores_avg = EXCLUDED.cpu_cores_avg, cpu_cores_max = EXCLUDED.cpu_cores_max,
				mem_bytes_avg = EXCLUDED.mem_bytes_avg, mem_bytes_max = EXCLUDED.mem_bytes_max,
				net_rx_bytes = EXCLUDED.net_rx_bytes, net_tx_bytes = EXCLUDED.net_tx_bytes,
				blk_read_bytes = EXCLUDED.blk_read_bytes, blk_write_bytes = EXCLUDED.blk_write_bytes`,
			ids[key(sm.Project, sm.Service)], start, sm.Samples, int(sm.State), sm.Counted, sm.Containers,
			sm.CPUAvg, sm.CPUMax, optInt64(sm.MemAvg), optInt64(sm.MemMax),
			int64(sm.NetRx), int64(sm.NetTx), int64(sm.BlkRead), int64(sm.BlkWrite))
	}
	for _, c := range m.Checks {
		b.Queue(`
			INSERT INTO sonar.check_minutes (service_id, minute, url, attempts, failures, slow, latency_ms_avg, latency_ms_max, state)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (service_id, minute) DO UPDATE SET
				url = EXCLUDED.url, attempts = EXCLUDED.attempts, failures = EXCLUDED.failures, slow = EXCLUDED.slow,
				latency_ms_avg = EXCLUDED.latency_ms_avg, latency_ms_max = EXCLUDED.latency_ms_max, state = EXCLUDED.state`,
			ids[key(c.Project, c.Service)], start, c.URL, c.Attempts, c.Failures, c.Slow, c.LatencyAvgMs, c.LatencyMaxMs, int(c.State))
	}
	for _, p := range m.Projects {
		b.Queue(`
			INSERT INTO sonar.project_minutes (project, minute, state, evidence) VALUES ($1, $2, $3, $4)
			ON CONFLICT (project, minute) DO UPDATE SET state = EXCLUDED.state, evidence = EXCLUDED.evidence`,
			p.Project, start, int(p.State), string(p.Evidence))
	}
	for _, u := range m.Updates {
		b.Queue(`
			INSERT INTO sonar.updates (service_id, started_at, ended_at, from_version, to_version, outcome)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (service_id, started_at) DO UPDATE SET
				ended_at = EXCLUDED.ended_at, from_version = EXCLUDED.from_version, to_version = EXCLUDED.to_version,
				outcome = EXCLUDED.outcome`,
			ids[key(u.Project, u.Service)], u.Started.UTC(), u.Ended.UTC(), u.From, u.To, u.Outcome)
	}

	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("database: writing minute %s: %w", start.Format(time.RFC3339), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: writing minute %s: %w", start.Format(time.RFC3339), err)
	}
	return nil
}

// upsertServices records every service the minute mentions and returns their
// IDs by project and service.
func upsertServices(ctx context.Context, tx pgx.Tx, m Minute) (map[string]int, error) {
	var keys [][2]string
	seen := make(map[string]bool)
	add := func(project, service string) {
		if k := key(project, service); !seen[k] {
			seen[k] = true
			keys = append(keys, [2]string{project, service})
		}
	}
	for _, s := range m.Services {
		add(s.Project, s.Service)
	}
	for _, c := range m.Checks {
		add(c.Project, c.Service)
	}
	for _, c := range m.Containers {
		add(c.Project, c.Service)
	}
	for _, u := range m.Updates {
		add(u.Project, u.Service)
	}

	start, end := m.Start.UTC(), m.Start.UTC().Add(time.Minute)
	b := &pgx.Batch{}
	for _, k := range keys {
		b.Queue(`
			INSERT INTO sonar.services (project, service, first_seen, last_seen) VALUES ($1, $2, $3, $4)
			ON CONFLICT (project, service) DO UPDATE SET
				first_seen = LEAST(services.first_seen, EXCLUDED.first_seen),
				last_seen = GREATEST(services.last_seen, EXCLUDED.last_seen)
			RETURNING id`, k[0], k[1], start, end)
	}
	results := tx.SendBatch(ctx, b)
	defer results.Close()

	ids := make(map[string]int, len(keys))
	for _, k := range keys {
		var id int
		if err := results.QueryRow().Scan(&id); err != nil {
			return nil, fmt.Errorf("database: recording service %s/%s: %w", k[0], k[1], err)
		}
		ids[key(k[0], k[1])] = id
	}
	return ids, results.Close()
}

func key(project, service string) string {
	return project + "\x00" + service
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func optInt64(v *uint64) *int64 {
	if v == nil {
		return nil
	}
	n := int64(*v)
	return &n
}
