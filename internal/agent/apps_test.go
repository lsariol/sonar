package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/report"
)

type fakeReports struct {
	mu      sync.Mutex
	reports map[string]report.Report
	errs    map[string]error
	fetched []string
}

func (f *fakeReports) Fetch(_ context.Context, container string, port int) (report.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, container)
	if err := f.errs[container]; err != nil {
		return report.Report{}, err
	}
	return f.reports[container], nil
}

func reporting(idc byte, project, port string) containers.Container {
	c := running(idc, project, project, "")
	c.ReportPort = port
	return c
}

// TestApps: counters grow by what changed, a restart starts them over, a
// failed reading counts nothing twice, and checks keep when they changed.
func TestApps(t *testing.T) {
	dibs := reporting('a', "dibs", "8080")
	d := &fakeDocker{list: []containers.Container{dibs, reporting('b', "plop", "http"), running('c', "lys", "lys", "")}}
	a, clk := newTestAgent(t, d, &fakeChecker{})
	started := clk.t.Add(-time.Hour)
	f := &fakeReports{reports: map[string]report.Report{"dibs": {StartedAt: started,
		Counters: map[string]uint64{"games_completed": 100},
		Checks:   []report.Check{{Name: "matchmaking", OK: true}}}}}
	a.opts.Reports = f
	a.collectors[appsCollector] = &collector{every: appsEvery}
	a.sampleDocker(context.Background())

	a.sampleApps(context.Background())
	if !slices.Equal(f.fetched, []string{"dibs"}) {
		t.Errorf("fetched %v, want only dibs", f.fetched)
	}
	m := a.bucketAt(clk.t).minute()
	if len(m.AppCounters) != 0 || len(m.AppChecks) != 1 {
		t.Errorf("first reading: %+v %+v; want the baseline only and the check first seen", m.AppCounters, m.AppChecks)
	}
	s := a.build()
	if !slices.ContainsFunc(s.Attention, func(m string) bool { return strings.Contains(m, "com.mobasity.report-port=\"http\"") }) {
		t.Errorf("attention = %v, want plop's bad label", s.Attention)
	}

	step := func(r report.Report, err error) {
		clk.t = clk.t.Add(time.Minute)
		f.reports["dibs"] = r
		f.errs = map[string]error{"dibs": err}
		a.sampleApps(context.Background())
	}
	step(report.Report{StartedAt: started, Counters: map[string]uint64{"games_completed": 104, "rematches": 2},
		Checks: []report.Check{{Name: "matchmaking", OK: true}}}, nil)
	m = a.bucketAt(clk.t).minute()
	if len(m.AppCounters) != 2 || m.AppCounters[0].Increase != 4 || m.AppCounters[1].Increase != 2 || len(m.AppChecks) != 0 {
		t.Errorf("second reading: %+v %+v; want +4 games, +2 rematches (new), no check change", m.AppCounters, m.AppChecks)
	}

	step(report.Report{}, errors.New("connection refused"))
	if m := a.bucketAt(clk.t).minute(); len(m.AppCounters) != 0 {
		t.Errorf("a failed reading counted: %+v", m.AppCounters)
	}
	since := clk.t.Add(-2 * time.Minute)
	step(report.Report{StartedAt: started, Counters: map[string]uint64{"games_completed": 110, "rematches": 2},
		Checks: []report.Check{{Name: "matchmaking", OK: false, Message: "queue stuck"}}}, nil)
	m = a.bucketAt(clk.t).minute()
	if len(m.AppCounters) != 1 || m.AppCounters[0].Increase != 6 || len(m.AppChecks) != 1 || m.AppChecks[0].OK {
		t.Errorf("after a failed reading: %+v %+v; want +6 and the check failing", m.AppCounters, m.AppChecks)
	}

	restartedAt := clk.t
	step(report.Report{StartedAt: restartedAt, Counters: map[string]uint64{"games_completed": 3},
		Checks: []report.Check{{Name: "matchmaking", OK: false, Message: "still stuck"}}}, nil)
	m = a.bucketAt(clk.t).minute()
	if len(m.AppCounters) != 1 || m.AppCounters[0].Increase != 3 || len(m.AppChecks) != 0 {
		t.Errorf("after a restart: %+v %+v; want +3", m.AppCounters, m.AppChecks)
	}
	s = a.build()
	if len(s.Apps) != 1 || s.Apps[0].Checks[0].Message != "still stuck" || s.Apps[0].Checks[0].Since.Before(since) {
		t.Errorf("apps = %+v", s.Apps)
	}

	// A new agent goes on where this one stopped.
	b, _ := newTestAgent(t, d, &fakeChecker{})
	b.opts.Reports, b.opts.Now = f, func() time.Time { return clk.t.Add(time.Minute) }
	b.collectors[appsCollector] = &collector{every: appsEvery}
	b.resumeFrom(s, clk.t)
	b.sampleDocker(context.Background())
	f.reports["dibs"] = report.Report{StartedAt: restartedAt, Counters: map[string]uint64{"games_completed": 5}}
	b.sampleApps(context.Background())
	if m := b.bucketAt(clk.t.Add(time.Minute)).minute(); len(m.AppCounters) != 1 || m.AppCounters[0].Increase != 2 {
		t.Errorf("after the agent's restart: %+v, want +2", m.AppCounters)
	}
}

// TestAppsFreshDeploy: a container that started moments ago counts its
// first reading.
func TestAppsFreshDeploy(t *testing.T) {
	d := &fakeDocker{list: []containers.Container{reporting('a', "dibs", "8080")}}
	a, clk := newTestAgent(t, d, &fakeChecker{})
	a.opts.Reports = &fakeReports{reports: map[string]report.Report{"dibs": {StartedAt: clk.t.Add(-30 * time.Second),
		Counters: map[string]uint64{"games_completed": 2}}}}
	a.collectors[appsCollector] = &collector{every: appsEvery}
	a.sampleDocker(context.Background())
	a.sampleApps(context.Background())
	if m := a.bucketAt(clk.t).minute(); len(m.AppCounters) != 1 || m.AppCounters[0].Increase != 2 {
		t.Errorf("a fresh deploy's first reading: %+v, want +2", m.AppCounters)
	}
}
