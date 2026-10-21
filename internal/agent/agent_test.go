package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/checks"
	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/host"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
)

var start = time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)

// clock is a fake time.Now that tests move forward.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type fakeDocker struct {
	list     []containers.Container
	stats    containers.StatsAnswer
	err      error
	statsErr error
}

func (f *fakeDocker) Containers(context.Context) ([]containers.Container, error) {
	return f.list, f.err
}

func (f *fakeDocker) Stats(context.Context) (containers.StatsAnswer, error) {
	return f.stats, f.statsErr
}

// fakeChecker answers each URL with its next queued outcome (ok when none),
// or refuses it as the guard would.
type fakeChecker struct {
	mu      sync.Mutex
	queue   map[string][]checks.Outcome
	refuse  map[string]bool
	checked []string
}

func (f *fakeChecker) Check(ctx context.Context, url string) checks.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, url)
	if f.refuse[url] {
		return checks.Result{At: start, Outcome: checks.Failed, Refused: true,
			Error: "localtest.me resolves to ::1, which isn't a public address, so it wasn't contacted"}
	}
	outcome := checks.OK
	if q := f.queue[url]; len(q) > 0 {
		outcome, f.queue[url] = q[0], q[1:]
	}
	r := checks.Result{At: start, Outcome: outcome, Status: 200, Latency: 40 * time.Millisecond}
	if outcome == checks.Failed {
		r.Status, r.Error = 502, "HTTP 502 Bad Gateway"
	}
	return r
}

// procDir is a copy of Spark's /proc samples that tests can change.
func procDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "proc"), 0o755)
	for _, name := range []string{"stat", "meminfo", "loadavg", "uptime"} {
		data, err := os.ReadFile(filepath.Join("..", "host", "testdata", "spark", "proc", name))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(root, "proc", name), data, 0o644)
	}
	return root
}

func newTestAgent(t *testing.T, d *fakeDocker, c *fakeChecker) (*Agent, *clock) {
	t.Helper()
	clk := &clock{t: start}
	a := New(Options{
		Version:  "v0.1.0",
		StateDir: t.TempDir(),
		Host:     host.Reader{Root: procDir(t)},
		DiskPath: "/",
		ReadDisk: func(string) (host.Disk, error) {
			return host.Disk{Total: 1000, Used: 120, Available: 860}, nil
		},
		Docker:  d,
		Checker: c,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     clk.now,
	})
	return a, clk
}

func id(c byte) string { return strings.Repeat(string(c), 64) }

func running(idc byte, project, service, url string) containers.Container {
	return containers.Container{ID: id(idc), Name: service, Project: project, Service: service, State: "running",
		Health: "none", RestartPolicy: "unless-stopped", PublicURL: url}
}

func TestHostCPU(t *testing.T) {
	a, clk := newTestAgent(t, &fakeDocker{}, &fakeChecker{})
	a.sampleHost()
	if s := a.build(); s.Host == nil || s.Host.CPUPercent != nil || math.Abs(s.Host.MemoryUsedPercent-20.49) > 0.01 {
		t.Fatalf("first reading: %+v; want memory, no CPU yet", s.Host)
	}

	// The container's later reading of the same counters, from Spark.
	stat := filepath.Join(a.opts.Host.Root, "proc", "stat")
	os.WriteFile(stat, []byte("cpu  23830475 25580 16322027 2659329865 3480505 0 577724 0 0 0\n"), 0o644)
	clk.t = clk.t.Add(hostEvery)
	a.sampleHost()

	h := a.build().Host
	if h.CPUPercent == nil || math.Abs(*h.CPUPercent-41.0/114*100) > 1e-9 {
		t.Errorf("CPU = %v, want 35.96", h.CPUPercent)
	}
	if h.CorePercents != nil {
		t.Errorf("cores = %v; the core lines vanished, so want none", h.CorePercents)
	}
}

func TestHostFailure(t *testing.T) {
	a, _ := newTestAgent(t, &fakeDocker{}, &fakeChecker{})
	os.Remove(filepath.Join(a.opts.Host.Root, "proc", "meminfo"))
	a.sampleHost()
	s := a.build()
	if s.Host != nil || s.Collectors[hostCollector].State != snapshot.Unavailable {
		t.Errorf("host = %+v, collector = %+v; want no host and unavailable", s.Host, s.Collectors[hostCollector])
	}
	if !slices.ContainsFunc(s.Attention, func(m string) bool { return strings.HasPrefix(m, "host: ") }) {
		t.Errorf("attention = %v, want the host problem", s.Attention)
	}
}

func TestContainerRates(t *testing.T) {
	plop := running('a', "plop", "plop", "")
	d := &fakeDocker{list: []containers.Container{plop}}
	a, clk := newTestAgent(t, d, &fakeChecker{})

	reading := func(at time.Time, cpu, rx uint64) containers.StatsAnswer {
		return containers.StatsAnswer{Stats: []containers.Stats{{ID: plop.ID, ReadAt: at, CPUNanos: cpu,
			MemoryBytes: 50 << 20, NetRxBytes: rx, NetTxBytes: 10, BlkReadBytes: 0, BlkWriteBytes: 4096}}}
	}
	d.stats = reading(start, 1_000_000_000, 5000)
	a.sampleDocker(context.Background())
	u := a.build().Projects[0].Services[0].Usage
	if u.CPUCores != nil || u.MemoryBytes == nil || *u.MemoryBytes != 50<<20 {
		t.Fatalf("first reading: %+v; want memory, no rates yet", u)
	}

	clk.t = start.Add(dockerEvery)
	d.stats = reading(start.Add(10*time.Second), 6_000_000_000, 6000)
	a.sampleDocker(context.Background())
	u = a.build().Projects[0].Services[0].Usage
	if u.CPUCores == nil || *u.CPUCores != 0.5 || *u.NetRxRate != 100 || *u.NetTxRate != 0 {
		t.Errorf("rates: cores %v rx %v tx %v; want 0.5, 100, 0", ptr(u.CPUCores), ptr(u.NetRxRate), ptr(u.NetTxRate))
	}

	clk.t = clk.t.Add(dockerEvery)
	d.stats = reading(start.Add(20*time.Second), 100, 10)
	a.sampleDocker(context.Background())
	if u := a.build().Projects[0].Services[0].Usage; u.CPUCores != nil || u.NetRxRate != nil {
		t.Errorf("after a restart (counters reset): %+v; want no rates", u)
	}
}

func ptr(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func TestProjectStates(t *testing.T) {
	plopURL := "https://plop.mobasity.com"
	job := containers.Container{ID: id('j'), Name: "plop-migrate", Project: "plop", Service: "migrate", State: "exited", RestartPolicy: "no"}
	stray := containers.Container{ID: id('s'), Name: "scratchpad", Service: "scratchpad", State: "exited", RestartPolicy: "no"}
	lys := running('l', "lys", "lys", "")
	lys.Health = "unhealthy"
	d := &fakeDocker{list: []containers.Container{running('p', "plop", "plop", plopURL), job, lys, stray}}
	c := &fakeChecker{queue: map[string][]checks.Outcome{plopURL: {checks.OK, checks.Failed, checks.Failed, checks.Slow}}}
	a, _ := newTestAgent(t, d, c)
	ctx := context.Background()
	a.sampleDocker(ctx)

	project := func(name string) snapshot.Project {
		for _, p := range a.build().Projects {
			if p.Project == name {
				return p
			}
		}
		t.Fatalf("no project %q", name)
		return snapshot.Project{}
	}

	if p := project("plop"); p.State != state.Up || p.Evidence != state.ContainerOnly {
		t.Errorf("before any check: %v %s, want up from Docker alone", p.State, p.Evidence)
	}
	steps := []state.State{state.Up, state.Up, state.Down, state.Degraded}
	for i, want := range steps {
		a.runChecks(ctx)
		if p := project("plop"); p.State != want || p.Evidence != state.Checked {
			t.Errorf("after check %d: %v %s, want %v checked", i+1, p.State, p.Evidence, want)
		}
	}

	if p := project("lys"); p.State != state.Down {
		t.Errorf("lys (its only service unhealthy) = %v, want down", p.State)
	}
	p := project("plop")
	for _, svc := range p.Services {
		if svc.Service == "migrate" && svc.Counted {
			t.Error("the one-off migrate job shouldn't count")
		}
	}
	if s := project(""); s.State != state.Unknown || len(s.Services) != 1 {
		t.Errorf("containers without compose labels: %+v", s)
	}
}

// TestRefusedAddress: a label that resolves somewhere private is a
// configuration problem to report, not an outage.
func TestRefusedAddress(t *testing.T) {
	trap := "https://localtest.me"
	d := &fakeDocker{list: []containers.Container{running('a', "plop", "plop", trap)}}
	c := &fakeChecker{refuse: map[string]bool{trap: true}}
	a, _ := newTestAgent(t, d, c)
	a.sampleDocker(context.Background())
	for range 3 {
		a.runChecks(context.Background())
	}

	s := a.build()
	if p := s.Projects[0]; p.State != state.Up || p.Evidence != state.ContainerOnly {
		t.Errorf("project = %v %s, want up from Docker alone", p.State, p.Evidence)
	}
	if ch := s.Checks[0]; ch.State != state.Unknown || ch.ConsecutiveFailures != 0 {
		t.Errorf("check = %+v, want unknown with no failures counted", ch)
	}
	if !slices.ContainsFunc(s.Attention, func(m string) bool { return strings.HasPrefix(m, "plop/plop: "+trap+": ") }) {
		t.Errorf("attention = %v, want the refused address", s.Attention)
	}
}

func TestDockerStale(t *testing.T) {
	d := &fakeDocker{list: []containers.Container{running('a', "plop", "plop", "")}}
	a, clk := newTestAgent(t, d, &fakeChecker{})
	a.sampleDocker(context.Background())

	d.err = errors.New("proxy: dial unix /run/sonar-proxy/proxy.sock: connection refused")
	clk.t = start.Add(30 * time.Second)
	a.sampleDocker(context.Background())
	if p := a.build().Projects[0]; p.State != state.Up {
		t.Errorf("30 s after the last list: %v, want the last state kept", p.State)
	}

	clk.t = start.Add(dockerFreshFor + time.Second)
	a.sampleDocker(context.Background())
	s := a.build()
	if p := s.Projects[0]; p.State != state.Unknown || p.Services[0].State != state.Unknown {
		t.Errorf("61 s after the last list: %v, want unknown", p.State)
	}
	if s.Collectors[dockerCollector].State != snapshot.Unavailable ||
		!slices.ContainsFunc(s.Attention, func(m string) bool { return strings.Contains(m, "connection refused") }) {
		t.Errorf("docker collector %+v, attention %v", s.Collectors[dockerCollector], s.Attention)
	}
}

func TestStatsFailureKeepsStates(t *testing.T) {
	d := &fakeDocker{list: []containers.Container{running('a', "plop", "plop", "")}, statsErr: errors.New("proxy: GET /v1/stats: timeout")}
	a, _ := newTestAgent(t, d, &fakeChecker{})
	a.sampleDocker(context.Background())
	s := a.build()
	if s.Projects[0].State != state.Up || s.Collectors[statsCollector].State != snapshot.Unavailable ||
		s.Collectors[dockerCollector].State != snapshot.Available {
		t.Errorf("project %v, stats %+v, docker %+v", s.Projects[0].State, s.Collectors[statsCollector], s.Collectors[dockerCollector])
	}
}

func TestLabels(t *testing.T) {
	good := running('a', "plop", "plop", "https://plop.mobasity.com")
	internal := running('b', "cove", "cove", "https://cove")
	replica := running('c', "plop", "plop", "https://plop.mobasity.com")
	d := &fakeDocker{list: []containers.Container{good, internal, replica}}
	c := &fakeChecker{}
	a, _ := newTestAgent(t, d, c)
	a.sampleDocker(context.Background())
	a.runChecks(context.Background())

	if !slices.Equal(c.checked, []string{"https://plop.mobasity.com"}) {
		t.Errorf("checked %v, want only plop's address, once", c.checked)
	}
	s := a.build()
	if !slices.ContainsFunc(s.Attention, func(m string) bool {
		return strings.HasPrefix(m, "cove/cove: label com.mobasity.public-url=") && strings.Contains(m, "internal name")
	}) {
		t.Errorf("attention = %v, want cove's label refused", s.Attention)
	}

	d.list = []containers.Container{internal}
	a.sampleDocker(context.Background())
	if s := a.build(); len(s.Checks) != 0 || len(a.checks) != 0 {
		t.Errorf("after plop's label went away: checks %v", s.Checks)
	}
}

func TestCollectorStates(t *testing.T) {
	a, clk := newTestAgent(t, &fakeDocker{}, &fakeChecker{})
	a.opts.ReadDisk = func(string) (host.Disk, error) {
		return host.Disk{}, errors.Join(errors.New("host: disk size of /"), errors.ErrUnsupported)
	}
	a.sampleHost()
	a.sampleDisk(context.Background())

	s := a.build()
	if s.Collectors[hostCollector].State != snapshot.Available || s.Collectors[diskCollector].State != snapshot.Unsupported ||
		s.Collectors[checksCollector].State != snapshot.Unavailable {
		t.Errorf("collectors = %+v", s.Collectors)
	}
	if slices.ContainsFunc(s.Attention, func(m string) bool { return strings.HasPrefix(m, "filesystem") }) {
		t.Errorf("an unsupported collector isn't a problem: %v", s.Attention)
	}

	clk.t = start.Add(3*hostEvery + time.Second)
	if st := a.build().Collectors[hostCollector]; st.State != snapshot.Stale {
		t.Errorf("host after 16 s without a reading: %+v, want stale", st)
	}
}

func TestWriteSnapshot(t *testing.T) {
	d := &fakeDocker{list: []containers.Container{running('a', "plop", "plop", "")}}
	a, _ := newTestAgent(t, d, &fakeChecker{})
	a.sampleHost()
	a.sampleDocker(context.Background())
	a.writeSnapshot()

	s, err := snapshot.Read(a.opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Fresh(start) || s.Version != "v0.1.0" || s.Host == nil || len(s.Projects) != 1 {
		t.Errorf("snapshot = %+v", s)
	}
}

func TestRun(t *testing.T) {
	d := &fakeDocker{list: []containers.Container{running('a', "plop", "plop", "https://plop.mobasity.com")}}
	c := &fakeChecker{}
	a, _ := newTestAgent(t, d, c)
	a.opts.Now = time.Now

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := snapshot.Read(a.opts.StateDir)
		if err == nil && len(s.Checks) == 1 && s.Checks[0].LastAt != nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no snapshot with a checked address after 5 s: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
}
