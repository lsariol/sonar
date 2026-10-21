package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lsariol/sonar/internal/state"
)

// The integration tests run against a real, disposable Postgres set up with
// scripts/db/setup.sql (scripts/test-db.sh does it). Without these
// variables they're skipped.
var (
	migratorURL = os.Getenv("SONAR_TEST_DATABASE_MIGRATOR_URL")
	appURL      = os.Getenv("SONAR_TEST_DATABASE_URL")
	readerURL   = os.Getenv("SONAR_TEST_DATABASE_READER_URL")
)

var migrateOnce sync.Once

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// open migrates once, connects as the app role and empties the tables.
func open(t *testing.T) *Store {
	t.Helper()
	if migratorURL == "" || appURL == "" || readerURL == "" {
		t.Skip("no test database: run scripts/test-db.sh up, then eval \"$(scripts/test-db.sh env)\"")
	}
	ctx := context.Background()
	var migrateErr error
	migrateOnce.Do(func() { migrateErr = Migrate(ctx, migratorURL, quiet) })
	if migrateErr != nil {
		t.Fatal(migrateErr)
	}

	s, err := Connect(ctx, appURL, "SONAR_TEST_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, table := range []string{"containers", "host_minutes", "service_minutes", "check_minutes",
		"project_minutes", "project_days", "service_days", "services"} {
		if _, err := s.pool.Exec(ctx, "DELETE FROM sonar."+table); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

var minute = time.Date(2026, 10, 21, 15, 4, 0, 0, time.UTC)

func f(v float64) *float64 { return &v }
func u(v uint64) *uint64   { return &v }
func i(v int) *int         { return &v }

func sampleMinute(at time.Time) Minute {
	started := at.Add(-time.Hour)
	return Minute{
		Start: at,
		Host: &HostMinute{Samples: 12, Expected: 12, CPUSamples: 12, CPUAvg: f(12.5), CPUMin: f(3), CPUMax: f(41),
			MemPctAvg: 20.5, MemPctMin: 20.4, MemPctMax: 20.6, MemUsedAvg: 3 << 30, MemTotal: 16 << 30, SwapUsedAvg: 512 << 10,
			Load1Avg: 0.4, Load1Max: 0.6, DiskUsed: u(28 << 30), DiskTotal: u(250 << 30)},
		Services: []ServiceMinute{
			{Project: "plop", Service: "plop", Samples: 6, State: state.Up, Counted: true, Containers: 1,
				CPUAvg: f(0.25), CPUMax: f(0.5), MemAvg: u(80 << 20), MemMax: u(90 << 20), NetRx: 6000, NetTx: 9000, BlkWrite: 4096},
			{Project: "plop", Service: "migrate", Samples: 6, State: state.Unknown, Counted: false},
		},
		Checks: []CheckMinute{{Project: "plop", Service: "plop", URL: "https://plop.mobasity.com", Attempts: 2, Slow: 1,
			LatencyAvgMs: i(900), LatencyMaxMs: i(2100), State: state.Degraded}},
		Projects: []ProjectMinute{{Project: "plop", State: state.Degraded, Evidence: state.Checked}},
		Containers: []ContainerSeen{{ID: strings.Repeat("a", 64), Project: "plop", Service: "plop", Name: "plop",
			Image: "plop-plop", Version: "v1.4.0", CreatedAt: started, StartedAt: &started}},
	}
}

func TestMigrations(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	have, want, err := s.SchemaVersion(ctx)
	if err != nil || have != want || want != 2 {
		t.Errorf("SchemaVersion = %d, %d, %v; want 2, 2", have, want, err)
	}
	if err := s.CheckSchemaVersion(ctx); err != nil {
		t.Error(err)
	}
	if err := Migrate(ctx, migratorURL, quiet); err != nil {
		t.Errorf("migrating again: %v", err)
	}

	var owner string
	s.pool.QueryRow(ctx, `SELECT tableowner FROM pg_tables WHERE schemaname = 'sonar' AND tablename = 'service_minutes'`).Scan(&owner)
	if owner != "sonar_owner" {
		t.Errorf("tables are owned by %q, want sonar_owner", owner)
	}
}

func TestWriteMinute(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.WriteMinute(ctx, sampleMinute(minute)); err != nil {
		t.Fatal(err)
	}

	var samples, cpuSamples int
	var cpu float64
	var disk int64
	s.pool.QueryRow(ctx, `SELECT samples, cpu_samples, cpu_pct_avg, disk_used_bytes FROM sonar.host_minutes WHERE minute = $1`, minute).
		Scan(&samples, &cpuSamples, &cpu, &disk)
	if samples != 12 || cpuSamples != 12 || cpu != 12.5 || disk != 28<<30 {
		t.Errorf("host row: %d %d %v %d", samples, cpuSamples, cpu, disk)
	}

	var svcState, rx int64
	var counted bool
	s.pool.QueryRow(ctx, `SELECT m.state, m.counted, m.net_rx_bytes FROM sonar.service_minutes m JOIN sonar.services s ON s.id = m.service_id
		WHERE s.project = 'plop' AND s.service = 'plop'`).Scan(&svcState, &counted, &rx)
	if svcState != int64(state.Up) || !counted || rx != 6000 {
		t.Errorf("service row: state %d counted %v rx %d", svcState, counted, rx)
	}

	var version string
	var firstSeen, lastSeen time.Time
	s.pool.QueryRow(ctx, `SELECT version, first_seen, last_seen FROM sonar.containers`).Scan(&version, &firstSeen, &lastSeen)
	if version != "v1.4.0" || !firstSeen.Equal(minute) || !lastSeen.Equal(minute.Add(time.Minute)) {
		t.Errorf("container row: %s %v %v", version, firstSeen, lastSeen)
	}

	// The same minute again (a retry) replaces it; the next minute extends the
	// container's last_seen.
	m := sampleMinute(minute)
	m.Host.Samples = 7
	if err := s.WriteMinute(ctx, m); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := s.WriteMinute(ctx, sampleMinute(minute.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	var rows int
	s.pool.QueryRow(ctx, `SELECT samples FROM sonar.host_minutes WHERE minute = $1`, minute).Scan(&samples)
	s.pool.QueryRow(ctx, `SELECT count(*) FROM sonar.services`).Scan(&rows)
	s.pool.QueryRow(ctx, `SELECT last_seen FROM sonar.containers`).Scan(&lastSeen)
	if samples != 7 || rows != 2 || !lastSeen.Equal(minute.Add(2*time.Minute)) {
		t.Errorf("after a retry and a second minute: samples %d, services %d, last_seen %v", samples, rows, lastSeen)
	}
}

func TestRollup(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

	states := []state.State{state.Up, state.Up, state.Up, state.Degraded, state.Down, state.Down}
	for n, st := range states {
		m := sampleMinute(day.Add(time.Duration(n) * time.Minute))
		m.Projects[0].State = st
		m.Checks[0].LatencyAvgMs = i(100 * (n + 1))
		m.Checks[0].Attempts, m.Checks[0].Failures = 2, 0
		if err := s.WriteMinute(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	other := sampleMinute(day.Add(-time.Minute))
	other.Projects[0].State = state.Down
	s.WriteMinute(ctx, other)

	if err := s.Rollup(ctx, day.Add(13*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollup(ctx, day); err != nil {
		t.Fatalf("again: %v", err)
	}

	var up, degraded, down, latency int
	err := s.pool.QueryRow(ctx, `SELECT minutes_up, minutes_degraded, minutes_down, latency_ms_avg FROM sonar.project_days
		WHERE project = 'plop' AND day = $1`, day).Scan(&up, &degraded, &down, &latency)
	if err != nil || up != 3 || degraded != 1 || down != 2 || latency != 350 {
		t.Errorf("plop on %s: %d up, %d degraded, %d down, %d ms, %v; want 3, 1, 2, 350", day.Format(time.DateOnly), up, degraded, down, latency, err)
	}

	var svcUp int
	s.pool.QueryRow(ctx, `SELECT d.minutes_up FROM sonar.service_days d JOIN sonar.services s ON s.id = d.service_id
		WHERE s.service = 'plop' AND d.day = $1`, day).Scan(&svcUp)
	var jobDays int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM sonar.service_days d JOIN sonar.services s ON s.id = d.service_id
		WHERE s.service = 'migrate'`).Scan(&jobDays)
	if svcUp != 6 || jobDays != 0 {
		t.Errorf("service days: plop up %d (want 6), rows for the job %d (want 0)", svcUp, jobDays)
	}
}

func TestPrune(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	old := sampleMinute(now.Add(-MinutesKeptFor - time.Hour))
	fresh := sampleMinute(now.Add(-MinutesKeptFor + time.Hour))
	for _, m := range []Minute{old, fresh} {
		if err := s.WriteMinute(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	s.pool.Exec(ctx, `INSERT INTO sonar.project_days VALUES ('plop', $1, 1, 0, 0, NULL), ('plop', $2, 1, 0, 0, NULL)`,
		now.Add(-DaysKeptFor-48*time.Hour), now.Add(-24*time.Hour))

	n, err := s.Prune(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Errorf("Prune removed %d rows, want 6: the old minute's host, 2 service, check and project rows, and the old day", n)
	}
	var minutes, days int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM sonar.host_minutes`).Scan(&minutes)
	s.pool.QueryRow(ctx, `SELECT count(*) FROM sonar.project_days`).Scan(&days)
	if minutes != 1 || days != 1 {
		t.Errorf("left: %d host minutes, %d days; want 1 and 1", minutes, days)
	}
}

// TestGrants checks each role gets the privileges it needs and nothing more.
func TestGrants(t *testing.T) {
	app := open(t)
	ctx := context.Background()
	reader, err := Connect(ctx, readerURL, "SONAR_TEST_DATABASE_READER_URL")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if err := app.WriteMinute(ctx, sampleMinute(minute)); err != nil {
		t.Fatalf("the app writes: %v", err)
	}
	var n int
	if err := reader.pool.QueryRow(ctx, `SELECT count(*) FROM sonar.service_minutes`).Scan(&n); err != nil || n != 2 {
		t.Errorf("the reader reads: %d, %v", n, err)
	}
	if err := reader.CheckSchemaVersion(ctx); err != nil {
		t.Errorf("the reader checks the schema: %v", err)
	}

	denied := []struct {
		who   *Store
		query string
	}{
		{reader, `DELETE FROM sonar.host_minutes`},
		{reader, `INSERT INTO sonar.project_days VALUES ('x', '2026-01-01', 0, 0, 0, NULL)`},
		{app, `DROP TABLE sonar.host_minutes`},
		{app, `CREATE TABLE sonar.extra (id int)`},
		{app, `DELETE FROM sonar.goose_db_version`},
		{app, `CREATE TABLE public.extra (id int)`},
	}
	for _, d := range denied {
		_, err := d.who.pool.Exec(ctx, d.query)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != "42501" && pgErr.Code != "42P01") {
			t.Errorf("%q: err = %v, want permission denied", d.query, err)
		}
	}
}

// TestReads checks the API's queries, logged in as the reader like the API.
func TestReads(t *testing.T) {
	app := open(t)
	ctx := context.Background()
	reader, err := Connect(ctx, readerURL, "SONAR_TEST_DATABASE_READER_URL")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	base := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	for n := range 6 {
		m := sampleMinute(base.Add(time.Duration(n) * time.Minute))
		m.Host.CPUAvg, m.Host.CPUMin, m.Host.CPUMax = f(float64(10*(n+1))), f(float64(n)), f(float64(90+n))
		m.Host.CPUSamples, m.Host.Samples = 12, 12
		if n == 5 {
			m.Host.CPUAvg, m.Host.CPUMin, m.Host.CPUMax, m.Host.CPUSamples, m.Host.Samples = nil, nil, nil, 0, 6
		}
		m.Checks[0].LatencyAvgMs, m.Checks[0].Attempts, m.Checks[0].Failures = i(100*(n+1)), 2, n%2
		if err := app.WriteMinute(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Rollup(ctx, base); err != nil {
		t.Fatal(err)
	}

	buckets, err := reader.HostHistory(ctx, base, base.Add(10*time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 || !buckets[0].Start.Equal(base) || !buckets[1].Start.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("buckets = %+v", buckets)
	}
	b0, b1 := buckets[0], buckets[1]
	if b0.Samples != 60 || *b0.CPUAvg != 30 || *b0.CPUMin != 0 || *b0.CPUMax != 94 {
		t.Errorf("first bucket: %d samples, cpu %v/%v/%v; want 60, 30/0/94", b0.Samples, *b0.CPUAvg, *b0.CPUMin, *b0.CPUMax)
	}
	if b1.Samples != 6 || b1.CPUAvg != nil || b1.MemPctAvg == nil {
		t.Errorf("second bucket (no CPU): %+v", b1)
	}

	days, err := reader.ProjectDays(ctx, base.AddDate(0, 0, -1), base)
	if err != nil || len(days) != 1 || days[0].Project != "plop" || days[0].Degraded != 6 || !days[0].Day.Equal(midnightUTC(base)) {
		t.Errorf("ProjectDays = %+v, %v", days, err)
	}

	resp, err := reader.ResponseTimes(ctx, base)
	// Weighted by answered checks: (100*2 + 200*1 + 300*2 + 400*1 + 500*2 + 600*1) / 9.
	if err != nil || resp["plop"] != 333 {
		t.Errorf("ResponseTimes = %v, %v; want plop 333", resp, err)
	}
}

func midnightUTC(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
