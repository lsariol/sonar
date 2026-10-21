package agent

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/checks"
	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/state"
	"github.com/lsariol/sonar/internal/store"
)

// fakeStore records written minutes, failing while fail is set.
type fakeStore struct {
	mu      sync.Mutex
	fail    error
	written []store.Minute
}

func (f *fakeStore) WriteMinute(ctx context.Context, m store.Minute) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.written = append(f.written, m)
	return nil
}

func (f *fakeStore) Rollup(context.Context, time.Time) error         { return nil }
func (f *fakeStore) Prune(context.Context, time.Time) (int64, error) { return 0, nil }

// aMinute runs one minute of readings starting at start: 12 host samples,
// 6 container readings (each 5e9 ns of CPU apart: half a core) and 2 checks,
// and a snapshot after each host sample.
func aMinute(t *testing.T, a *Agent, clk *clock, d *fakeDocker, from time.Time, cpuFrom uint64) {
	t.Helper()
	plop := d.list[0].ID
	clk.t = from
	a.sampleDisk(context.Background())
	for n := range 12 {
		clk.t = from.Add(time.Duration(n) * hostEvery)
		a.sampleHost()
		if n%2 == 0 {
			d.stats = containers.StatsAnswer{Stats: []containers.Stats{{ID: plop, ReadAt: clk.t,
				CPUNanos: cpuFrom + uint64(n)*2_500_000_000, MemoryBytes: 100 << 20, NetRxBytes: 1000 * uint64(n), NetTxBytes: 0}}}
			a.sampleDocker(context.Background())
		}
		if n%6 == 0 {
			a.runChecks(context.Background())
		}
		a.writeSnapshot()
	}
}

func TestMinuteTotals(t *testing.T) {
	url := "https://plop.mobasity.com"
	d := &fakeDocker{list: []containers.Container{running('a', "plop", "plop", url)}}
	c := &fakeChecker{queue: map[string][]checks.Outcome{url: {checks.OK, checks.Slow}}}
	a, clk := newTestAgent(t, d, c)
	minuteStart := start.Truncate(time.Minute)
	aMinute(t, a, clk, d, minuteStart, 0)

	clk.t = minuteStart.Add(time.Minute + flushDelay - time.Second)
	a.closeMinutes(clk.t)
	if len(a.pending) != 0 {
		t.Fatal("closed a minute before its flush delay")
	}
	clk.t = minuteStart.Add(time.Minute + flushDelay)
	a.closeMinutes(clk.t)
	if len(a.pending) != 1 {
		t.Fatalf("%d minutes pending, want 1", len(a.pending))
	}
	m := a.pending[0]

	if !m.Start.Equal(minuteStart) || m.Host == nil || m.Host.Samples != 12 || m.Host.Expected != 12 {
		t.Fatalf("host minute = %+v", m.Host)
	}
	if m.Host.CPUSamples != 0 || m.Host.CPUAvg != nil {
		t.Errorf("CPU: Spark's sample doesn't change between readings, so no CPU figure: %+v", m.Host)
	}
	if math.Abs(m.Host.MemPctAvg-20.49) > 0.01 || m.Host.DiskTotal == nil || *m.Host.DiskTotal != 1000 {
		t.Errorf("host minute = %+v", m.Host)
	}

	if len(m.Services) != 1 {
		t.Fatalf("services = %+v", m.Services)
	}
	s := m.Services[0]
	if s.Samples != 6 || s.State != state.Up || !s.Counted || s.Containers != 1 {
		t.Errorf("service minute = %+v", s)
	}
	if s.CPUAvg == nil || *s.CPUAvg != 0.5 || s.MemAvg == nil || *s.MemAvg != 100<<20 {
		t.Errorf("usage: cores %v, memory %v; want 0.5 and 100 MiB", ptr(s.CPUAvg), s.MemAvg)
	}
	if s.NetRx != 10000 {
		t.Errorf("network in during the minute = %d, want 10000 (0 → 10000)", s.NetRx)
	}

	if len(m.Checks) != 1 || m.Checks[0].Attempts != 2 || m.Checks[0].Slow != 1 || m.Checks[0].State != state.Degraded ||
		m.Checks[0].LatencyAvgMs == nil || *m.Checks[0].LatencyAvgMs != 40 {
		t.Errorf("check minute = %+v", m.Checks)
	}
	if len(m.Projects) != 1 || m.Projects[0].State != state.Degraded || m.Projects[0].Evidence != state.Checked {
		t.Errorf("project minute = %+v; the slow check made it degraded", m.Projects)
	}
	if len(m.Containers) != 1 || m.Containers[0].Project != "plop" {
		t.Errorf("containers = %+v", m.Containers)
	}
}

func TestFlushRetriesInOrder(t *testing.T) {
	d := &fakeDocker{list: []containers.Container{running('a', "plop", "plop", "")}}
	a, clk := newTestAgent(t, d, &fakeChecker{})
	a.collectors[databaseCollector] = &collector{every: time.Minute}
	db := &fakeStore{fail: errors.New("database: writing minute: connection refused")}

	first := start.Truncate(time.Minute)
	for n := range 3 {
		aMinute(t, a, clk, d, first.Add(time.Duration(n)*time.Minute), uint64(n)*60e9)
	}
	clk.t = first.Add(3*time.Minute + flushDelay)
	a.flush(context.Background(), db)
	s := a.build()
	if len(a.pending) != 3 || s.Collectors[databaseCollector].State != "unavailable" {
		t.Fatalf("while the database is down: %d pending, collector %+v", len(a.pending), s.Collectors[databaseCollector])
	}
	if err := s.Collectors[databaseCollector].Error; err == "" || !strings.Contains(err, "3 minutes waiting") {
		t.Errorf("error = %q, want the queue length", err)
	}

	db.fail = nil
	a.flush(context.Background(), db)
	if len(a.pending) != 0 || len(db.written) != 3 {
		t.Fatalf("after recovery: %d pending, %d written", len(a.pending), len(db.written))
	}
	for n, m := range db.written {
		if !m.Start.Equal(first.Add(time.Duration(n) * time.Minute)) {
			t.Errorf("written out of order: %v", m.Start)
		}
	}
	if st := a.build().Collectors[databaseCollector].State; st != "available" {
		t.Errorf("collector after recovery: %s", st)
	}
}

func TestPendingCap(t *testing.T) {
	a, _ := newTestAgent(t, &fakeDocker{}, &fakeChecker{})
	first := start.Truncate(time.Minute)
	for n := range maxPending + 5 {
		a.bucketAt(first.Add(time.Duration(n) * time.Minute))
	}
	a.closeMinutes(first.Add(time.Duration(maxPending+6) * time.Minute))
	if len(a.pending) != maxPending || !a.pending[0].Start.Equal(first.Add(5*time.Minute)) {
		t.Errorf("%d pending from %v; want %d, the 5 oldest dropped", len(a.pending), a.pending[0].Start, maxPending)
	}
}

// TestWritesToDatabase runs a minute through to a real database when
// scripts/test-db.sh is up.
func TestWritesToDatabase(t *testing.T) {
	migrator, app := os.Getenv("SONAR_TEST_DATABASE_MIGRATOR_URL"), os.Getenv("SONAR_TEST_DATABASE_URL")
	if migrator == "" || app == "" {
		t.Skip("no test database: run scripts/test-db.sh up, then eval \"$(scripts/test-db.sh env)\"")
	}
	ctx := context.Background()
	d := &fakeDocker{list: []containers.Container{running('z', "agent-test", "web", "")}}
	a, clk := newTestAgent(t, d, &fakeChecker{})
	a.collectors[databaseCollector] = &collector{every: time.Minute}
	if err := store.Migrate(ctx, migrator, a.opts.Log); err != nil {
		t.Fatal(err)
	}
	db, err := store.Connect(ctx, app, "SONAR_TEST_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	from := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	aMinute(t, a, clk, d, from, 0)
	clk.t = from.Add(time.Minute + flushDelay)
	a.flush(ctx, db)
	if len(a.pending) != 0 {
		t.Fatalf("not written: %+v", a.build().Collectors[databaseCollector])
	}
	if err := db.Rollup(ctx, from); err != nil {
		t.Fatal(err)
	}
}
