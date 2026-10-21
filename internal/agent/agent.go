// Package agent is sonar-agent: it samples the host and every container,
// checks public addresses, and writes the snapshot the API reads. Each
// collector fails on its own, and a failure never becomes a zero.
package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/lsariol/sonar/internal/checks"
	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/host"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
)

const (
	hostEvery   = 5 * time.Second
	diskEvery   = 30 * time.Second
	dockerEvery = 10 * time.Second
	checksEvery = 30 * time.Second

	// dockerFreshFor is how long the last container list stands in for
	// failed ones before projects count as unknown.
	dockerFreshFor = 60 * time.Second

	dockerTimeout = 20 * time.Second
	checkTimeout  = 10 * time.Second
	checkParallel = 8
)

// Collector names, as the snapshot and `sonar status` show them.
const (
	hostCollector   = "host"
	diskCollector   = "filesystem"
	dockerCollector = "docker"
	statsCollector  = "stats"
	checksCollector = "checks"
)

// Docker is where the agent gets containers and their stats: sonar-proxy.
type Docker interface {
	Containers(ctx context.Context) ([]containers.Container, error)
	Stats(ctx context.Context) (containers.StatsAnswer, error)
}

// Checker runs one outside check.
type Checker interface {
	Check(ctx context.Context, url string) checks.Result
}

// Options are the agent's parts; tests swap them for fakes.
type Options struct {
	Version  string
	StateDir string
	Host     host.Reader
	DiskPath string
	ReadDisk func(string) (host.Disk, error) // host.ReadDisk unless set
	Docker   Docker
	Checker  Checker
	Log      *slog.Logger
	Now      func() time.Time // time.Now unless set
}

// Agent collects and writes the snapshot.
type Agent struct {
	opts Options

	mu         sync.Mutex
	collectors map[string]*collector
	writeErr   string

	cpu      host.CPU
	cpuPct   *float64
	corePcts []float64
	memory   host.Memory
	load     host.Load
	uptime   time.Duration
	hostAt   time.Time
	disk     *host.Disk

	containers []containers.Container
	readings   map[string]containers.Stats // last stats per container ID
	usage      map[string]snapshot.Usage

	targets []target
	refused []string // labels that can't be checked, for attention
	checks  map[target]*checkStatus
}

// collector is one source of data and how it's doing.
type collector struct {
	every  time.Duration
	lastOK time.Time
	err    error
}

// target is one public address to check, and whose it is.
type target struct {
	project, service, url string
}

type checkStatus struct {
	state    state.State
	failures int // in a row
	last     checks.Result
}

// New returns an agent; Run starts it.
func New(opts Options) *Agent {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ReadDisk == nil {
		opts.ReadDisk = host.ReadDisk
	}
	return &Agent{
		opts: opts,
		collectors: map[string]*collector{
			hostCollector:   {every: hostEvery},
			diskCollector:   {every: diskEvery},
			dockerCollector: {every: dockerEvery},
			statsCollector:  {every: dockerEvery},
			checksCollector: {every: checksEvery},
		},
		readings: make(map[string]containers.Stats),
		usage:    make(map[string]snapshot.Usage),
		checks:   make(map[target]*checkStatus),
	}
}

// Run collects and writes the snapshot until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(a.opts.StateDir, 0o755); err != nil {
		return fmt.Errorf("agent: %w: is the state volume mounted?", err)
	}
	a.sampleDisk(ctx)
	a.sampleDocker(ctx)

	var wg sync.WaitGroup
	every := func(d time.Duration, now bool, fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if now {
				fn(ctx)
			}
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fn(ctx)
				}
			}
		}()
	}
	every(diskEvery, false, a.sampleDisk)
	every(dockerEvery, false, a.sampleDocker)
	every(checksEvery, true, a.runChecks)
	every(hostEvery, true, func(context.Context) {
		a.sampleHost()
		a.writeSnapshot()
	})
	wg.Wait()
	return nil
}

func (a *Agent) sampleHost() {
	r := a.opts.Host
	cpu, errCPU := r.CPU()
	mem, errMem := r.Memory()
	load, errLoad := r.Load()
	uptime, errUp := r.Uptime()

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := errors.Join(errCPU, errMem, errLoad, errUp); err != nil {
		a.record(hostCollector, err)
		return
	}

	a.cpuPct, a.corePcts = nil, nil
	if !a.hostAt.IsZero() {
		if pct, ok := host.Busy(a.cpu.Total, cpu.Total); ok {
			a.cpuPct = &pct
		}
		if len(cpu.Cores) == len(a.cpu.Cores) {
			pcts := make([]float64, 0, len(cpu.Cores))
			for i := range cpu.Cores {
				if pct, ok := host.Busy(a.cpu.Cores[i], cpu.Cores[i]); ok {
					pcts = append(pcts, pct)
				}
			}
			if len(pcts) == len(cpu.Cores) {
				a.corePcts = pcts
			}
		}
	}
	a.cpu, a.memory, a.load, a.uptime, a.hostAt = cpu, mem, load, uptime, a.opts.Now()
	a.record(hostCollector, nil)
}

func (a *Agent) sampleDisk(context.Context) {
	d, err := a.opts.ReadDisk(a.opts.DiskPath)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.disk = &d
	}
	a.record(diskCollector, err)
}

func (a *Agent) sampleDocker(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	list, err := a.opts.Docker.Containers(ctx)
	var stats containers.StatsAnswer
	var statsErr error
	if err == nil {
		stats, statsErr = a.opts.Docker.Stats(ctx)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.record(dockerCollector, err)
	if err != nil {
		return
	}
	a.containers = list
	a.updateTargets()
	a.record(statsCollector, statsErr)
	if statsErr == nil {
		a.updateUsage(stats)
	}
}

// updateUsage turns two readings of each container's counters into rates.
// A container whose stats failed keeps its last reading for next time.
func (a *Agent) updateUsage(answer containers.StatsAnswer) {
	readings := make(map[string]containers.Stats, len(answer.Stats))
	usage := make(map[string]snapshot.Usage, len(answer.Stats))
	for _, s := range answer.Stats {
		u := snapshot.Usage{
			MemoryBytes:   &s.MemoryBytes,
			NetRxBytes:    &s.NetRxBytes,
			NetTxBytes:    &s.NetTxBytes,
			BlkReadBytes:  &s.BlkReadBytes,
			BlkWriteBytes: &s.BlkWriteBytes,
		}
		if prev, ok := a.readings[s.ID]; ok {
			if secs := s.ReadAt.Sub(prev.ReadAt).Seconds(); secs > 0 {
				u.CPUCores = rate(prev.CPUNanos, s.CPUNanos, secs*1e9)
				u.NetRxRate = rate(prev.NetRxBytes, s.NetRxBytes, secs)
				u.NetTxRate = rate(prev.NetTxBytes, s.NetTxBytes, secs)
				u.BlkReadRate = rate(prev.BlkReadBytes, s.BlkReadBytes, secs)
				u.BlkWriteRate = rate(prev.BlkWriteBytes, s.BlkWriteBytes, secs)
			}
		}
		readings[s.ID], usage[s.ID] = s, u
	}
	for _, f := range answer.Failed {
		if prev, ok := a.readings[f.ID]; ok {
			readings[f.ID] = prev
		}
	}
	a.readings, a.usage = readings, usage
}

// rate is how fast a counter grew, per unit; nil when it went backwards (the
// container restarted).
func rate(prev, cur uint64, per float64) *float64 {
	if cur < prev || per <= 0 {
		return nil
	}
	v := float64(cur-prev) / per
	return &v
}

// updateTargets reads the public-url labels: valid ones become targets,
// the rest are reported. Checks of labels that went away are forgotten.
func (a *Agent) updateTargets() {
	var targets []target
	var refused []string
	seen := make(map[target]bool)
	for _, c := range a.containers {
		if c.PublicURL == "" {
			continue
		}
		t := target{project: c.Project, service: c.Service, url: c.PublicURL}
		if seen[t] {
			continue
		}
		seen[t] = true
		if _, err := checks.Validate(c.PublicURL); err != nil {
			refused = append(refused, fmt.Sprintf("%s: label %s=%q %s", whose(c.Project, c.Service), containers.PublicURLLabel, c.PublicURL, err))
			continue
		}
		targets = append(targets, t)
	}
	slices.SortFunc(targets, func(x, y target) int {
		return cmp.Or(cmp.Compare(x.project, y.project), cmp.Compare(x.service, y.service), cmp.Compare(x.url, y.url))
	})
	for t := range a.checks {
		if !slices.Contains(targets, t) {
			delete(a.checks, t)
		}
	}
	a.targets, a.refused = targets, refused
}

func (a *Agent) runChecks(ctx context.Context) {
	a.mu.Lock()
	targets := slices.Clone(a.targets)
	a.mu.Unlock()

	results := make([]checks.Result, len(targets))
	var wg sync.WaitGroup
	slots := make(chan struct{}, checkParallel)
	for i, t := range targets {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			ctx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			results[i] = a.opts.Checker.Check(ctx, t.url)
		}()
	}
	wg.Wait()

	a.mu.Lock()
	defer a.mu.Unlock()
	for i, t := range targets {
		if !slices.Contains(a.targets, t) {
			continue
		}
		st := a.checks[t]
		if st == nil {
			st = &checkStatus{}
			a.checks[t] = st
		}
		r := results[i]
		st.last = r
		if r.Refused {
			// A label pointing somewhere Sonar won't go says nothing about
			// the service; attention reports it.
			st.state, st.failures = state.Unknown, 0
			continue
		}
		if r.Outcome == checks.Failed {
			st.failures++
		} else {
			st.failures = 0
		}
		st.state = state.Check(st.state, st.failures, r.Outcome == checks.Slow)
	}
	a.record(checksCollector, nil)
}

// record notes a collector's result, and logs when its error changes.
// Callers hold a.mu.
func (a *Agent) record(name string, err error) {
	c := a.collectors[name]
	switch {
	case err == nil && c.err != nil:
		a.opts.Log.Info("collector recovered", "collector", name)
	case err != nil && (c.err == nil || c.err.Error() != err.Error()):
		a.opts.Log.Warn("collector failed", "collector", name, "err", err)
	}
	if err == nil {
		c.lastOK = a.opts.Now()
	}
	c.err = err
}

func (a *Agent) writeSnapshot() {
	a.mu.Lock()
	s := a.build()
	a.mu.Unlock()

	err := snapshot.Write(a.opts.StateDir, s)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if msg != a.writeErr {
		if err != nil {
			a.opts.Log.Error("writing the snapshot failed", "err", err)
		} else {
			a.opts.Log.Info("writing the snapshot works again")
		}
		a.writeErr = msg
	}
}

func whose(project, service string) string {
	if project == "" {
		return service
	}
	return project + "/" + service
}
