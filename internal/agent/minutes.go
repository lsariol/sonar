package agent

import (
	"cmp"
	"slices"
	"time"

	"github.com/lsariol/sonar/internal/checks"
	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/host"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
	"github.com/lsariol/sonar/internal/store"
)

// hostSamplesPerMinute is how many host readings a full minute has.
const hostSamplesPerMinute = int(time.Minute / hostEvery)

// bucket adds up one minute of readings, filed under the minute they were recorded in.
type bucket struct {
	start      time.Time
	host       hostSum
	services   map[serviceKey]*serviceSum
	checks     map[serviceKey]*checkSum
	projects   map[string]*projectSum
	containers map[string]store.ContainerSeen
	network    map[string]*counters // bytes each host interface moved
	databases  []store.DatabaseRow  // the minute's last reading of sparkdb
	disk       *snapshot.DiskUse    // a new measurement of disk use
	traffic    []store.TrafficHour  // hours read from Cloudflare

	appCounters map[appCounterKey]uint64 // how much each counter grew
	appChecks   []store.AppCheckChange   // checks that changed
	updates     []store.Update           // updates that ended during the minute
}

type serviceKey struct{ project, service string }

type hostSum struct {
	samples, cpuSamples          int
	cpuSum, cpuMin, cpuMax       float64
	memPctSum, memPctMin, memMax float64
	memUsedSum, memTotal         uint64
	swapSum                      uint64
	loadSum, loadMax             float64
	disk                         *host.Disk
}

type serviceSum struct {
	samples, containers int
	state               state.State
	counted             bool
	cpuSum, cpuMax      float64
	cpuN                int
	memSum, memMax      uint64
	memN                int
	netRx, netTx        uint64
	blkRead, blkWrite   uint64
}

type checkSum struct {
	url                      string
	attempts, failures, slow int
	latSum, latMax           int64
	latN                     int
	state                    state.State
	lastError                string
}

type projectSum struct {
	state    state.State
	evidence state.Evidence
}

// counters is how much a container's counters grew since its last reading.
type counters struct {
	netRx, netTx, blkRead, blkWrite uint64
}

// bucketAt returns the open bucket for the minute holding t. Callers hold a.mu.
func (a *Agent) bucketAt(t time.Time) *bucket {
	start := t.UTC().Truncate(time.Minute)
	b := a.buckets[start]
	if b == nil {
		b = &bucket{
			start:      start,
			services:   make(map[serviceKey]*serviceSum),
			checks:     make(map[serviceKey]*checkSum),
			projects:   make(map[string]*projectSum),
			containers: make(map[string]store.ContainerSeen),
			network:    make(map[string]*counters),

			appCounters: make(map[appCounterKey]uint64),
		}
		a.buckets[start] = b
	}
	return b
}

func (a *Agent) addHostMinute(now time.Time) {
	h := &a.bucketAt(now).host
	memPct := a.memory.UsedPercent()
	if h.samples == 0 {
		h.memPctMin, h.memMax = memPct, memPct
	}
	h.samples++
	h.memPctSum += memPct
	h.memPctMin, h.memMax = min(h.memPctMin, memPct), max(h.memMax, memPct)
	h.memUsedSum += a.memory.Used()
	h.memTotal = a.memory.Total
	h.swapSum += a.memory.SwapUsed()
	h.loadSum += a.load.One
	h.loadMax = max(h.loadMax, a.load.One)
	h.disk = a.disk

	if a.cpuPct != nil {
		cpu := *a.cpuPct
		if h.cpuSamples == 0 {
			h.cpuMin, h.cpuMax = cpu, cpu
		}
		h.cpuSamples++
		h.cpuSum += cpu
		h.cpuMin, h.cpuMax = min(h.cpuMin, cpu), max(h.cpuMax, cpu)
	}
}

// addDockerMinute files one container reading: states, usage, counter growth and the containers seen.
func (a *Agent) addDockerMinute(now time.Time, grown map[string]counters) {
	b := a.bucketAt(now)
	byService := make(map[serviceKey][]containers.Container)
	for _, c := range a.containers {
		k := serviceKey{c.Project, c.Service}
		byService[k] = append(byService[k], c)
		b.containers[c.ID] = store.ContainerSeen{ID: c.ID, Project: c.Project, Service: c.Service, Name: c.Name,
			Image: c.Image, Version: c.Version, CreatedAt: c.CreatedAt, StartedAt: c.StartedAt, FinishedAt: c.FinishedAt,
			ExitCode: c.ExitCode}
	}

	for k, cs := range byService {
		s := b.services[k]
		if s == nil {
			s = &serviceSum{}
			b.services[k] = s
		}
		st, counted := a.serviceState(k, cs, now)
		s.samples++
		s.state = state.Worst(s.state, st)
		s.counted = s.counted || counted
		s.containers = max(s.containers, len(cs))

		var usages []snapshot.Usage
		for _, c := range cs {
			usages = append(usages, a.usage[c.ID])
			g := grown[c.ID]
			s.netRx += g.netRx
			s.netTx += g.netTx
			s.blkRead += g.blkRead
			s.blkWrite += g.blkWrite
		}
		u := sumUsage(usages)
		if u.CPUCores != nil {
			s.cpuSum += *u.CPUCores
			s.cpuMax = max(s.cpuMax, *u.CPUCores)
			s.cpuN++
		}
		if u.MemoryBytes != nil {
			s.memSum += *u.MemoryBytes
			s.memMax = max(s.memMax, *u.MemoryBytes)
			s.memN++
		}
	}
}

// addNetworkMinute files the bytes each host interface moved; callers hold a.mu.
func (a *Agent) addNetworkMinute(now time.Time, grown map[string]counters) {
	b := a.bucketAt(now)
	for name, g := range grown {
		n := b.network[name]
		if n == nil {
			n = &counters{}
			b.network[name] = n
		}
		n.netRx += g.netRx
		n.netTx += g.netTx
	}
}

// addCheckMinute files one check: prev is the state it held since the last check, st its state now.
func (a *Agent) addCheckMinute(now time.Time, t target, r checks.Result, prev, st state.State) {
	b := a.bucketAt(now)
	k := serviceKey{t.project, t.service}
	c := b.checks[k]
	if c == nil {
		// The state carried in from the last minute held until now, so it's part of this minute too.
		c = &checkSum{state: prev}
		b.checks[k] = c
	}
	c.url, c.state = t.url, state.Worst(c.state, st)
	c.attempts++
	switch r.Outcome {
	case checks.Failed:
		c.failures++
		c.lastError = r.Error
	case checks.Slow:
		c.slow++
	}
	if r.Outcome != checks.Failed && r.Latency > 0 {
		ms := r.Latency.Milliseconds()
		c.latSum += ms
		c.latMax = max(c.latMax, ms)
		c.latN++
	}
}

// addProjectMinute files the projects' states from one snapshot; unknown states are gaps.
func (a *Agent) addProjectMinute(now time.Time, projects []snapshot.Project) {
	b := a.bucketAt(now)
	for _, p := range projects {
		if p.State == state.Unknown || p.Project == "" {
			continue
		}
		s := b.projects[p.Project]
		if s == nil {
			s = &projectSum{}
			b.projects[p.Project] = s
		}
		s.state = state.Worst(s.state, p.State)
		if p.Evidence == state.Checked || s.evidence == "" {
			s.evidence = p.Evidence
		}
	}
}

// closeMinutes queues every bucket that ended more than flushDelay ago, oldest first; callers hold a.mu.
func (a *Agent) closeMinutes(now time.Time) {
	var done []time.Time
	for start := range a.buckets {
		if !now.Before(start.Add(time.Minute + flushDelay)) {
			done = append(done, start)
		}
	}
	slices.SortFunc(done, func(x, y time.Time) int { return x.Compare(y) })
	for _, start := range done {
		a.pending = append(a.pending, a.buckets[start].minute())
		delete(a.buckets, start)
	}
	if over := len(a.pending) - maxPending; over > 0 {
		a.opts.Log.Error("the database has been unreachable too long: dropping the oldest minutes",
			"dropped", over, "from", a.pending[0].Start.Format(time.RFC3339))
		a.pending = a.pending[over:]
	}
}

// minute turns the bucket into what the store writes.
func (b *bucket) minute() store.Minute {
	m := store.Minute{Start: b.start}

	if h := b.host; h.samples > 0 {
		n := float64(h.samples)
		hm := &store.HostMinute{
			Samples: h.samples, Expected: hostSamplesPerMinute, CPUSamples: h.cpuSamples,
			MemPctAvg: h.memPctSum / n, MemPctMin: h.memPctMin, MemPctMax: h.memMax,
			MemUsedAvg: h.memUsedSum / uint64(h.samples), MemTotal: h.memTotal, SwapUsedAvg: h.swapSum / uint64(h.samples),
			Load1Avg: h.loadSum / n, Load1Max: h.loadMax,
		}
		if h.cpuSamples > 0 {
			avg, lo, hi := h.cpuSum/float64(h.cpuSamples), h.cpuMin, h.cpuMax
			hm.CPUAvg, hm.CPUMin, hm.CPUMax = &avg, &lo, &hi
		}
		if h.disk != nil {
			used, total := h.disk.Used, h.disk.Total
			hm.DiskUsed, hm.DiskTotal = &used, &total
		}
		m.Host = hm
	}

	for k, s := range b.services {
		sm := store.ServiceMinute{Project: k.project, Service: k.service, Samples: s.samples, State: s.state,
			Counted: s.counted, Containers: s.containers,
			NetRx: s.netRx, NetTx: s.netTx, BlkRead: s.blkRead, BlkWrite: s.blkWrite}
		if s.cpuN > 0 {
			avg, hi := s.cpuSum/float64(s.cpuN), s.cpuMax
			sm.CPUAvg, sm.CPUMax = &avg, &hi
		}
		if s.memN > 0 {
			avg, hi := s.memSum/uint64(s.memN), s.memMax
			sm.MemAvg, sm.MemMax = &avg, &hi
		}
		m.Services = append(m.Services, sm)
	}
	for k, c := range b.checks {
		cm := store.CheckMinute{Project: k.project, Service: k.service, URL: c.url, Attempts: c.attempts,
			Failures: c.failures, Slow: c.slow, State: c.state, LastError: c.lastError}
		if c.latN > 0 {
			avg, hi := int(c.latSum/int64(c.latN)), int(c.latMax)
			cm.LatencyAvgMs, cm.LatencyMaxMs = &avg, &hi
		}
		m.Checks = append(m.Checks, cm)
	}
	for name, p := range b.projects {
		m.Projects = append(m.Projects, store.ProjectMinute{Project: name, State: p.state, Evidence: p.evidence})
	}
	for _, c := range b.containers {
		m.Containers = append(m.Containers, c)
	}
	m.Updates = b.updates
	for name, n := range b.network {
		m.Network = append(m.Network, store.NetMinute{Interface: name, RxBytes: n.netRx, TxBytes: n.netTx})
	}
	slices.SortFunc(m.Network, func(x, y store.NetMinute) int { return cmp.Compare(x.Interface, y.Interface) })
	m.Disk = diskMeasurement(b.disk)
	m.Traffic = b.traffic
	m.AppChecks = b.appChecks
	for k, v := range b.appCounters {
		m.AppCounters = append(m.AppCounters, store.AppCounter{Project: k.project, Name: k.name, Increase: v})
	}
	slices.SortFunc(m.AppCounters, func(x, y store.AppCounter) int {
		return cmp.Or(cmp.Compare(x.Project, y.Project), cmp.Compare(x.Name, y.Name))
	})
	for _, d := range b.databases {
		m.Databases = append(m.Databases, store.DatabaseMinute{Database: d.Name, Connections: d.Connections, Active: d.Active,
			SizeBytes: d.SizeBytes})
	}

	slices.SortFunc(m.Services, func(x, y store.ServiceMinute) int {
		return cmp.Or(cmp.Compare(x.Project, y.Project), cmp.Compare(x.Service, y.Service))
	})
	slices.SortFunc(m.Checks, func(x, y store.CheckMinute) int {
		return cmp.Or(cmp.Compare(x.Project, y.Project), cmp.Compare(x.Service, y.Service))
	})
	slices.SortFunc(m.Projects, func(x, y store.ProjectMinute) int { return cmp.Compare(x.Project, y.Project) })
	slices.SortFunc(m.Containers, func(x, y store.ContainerSeen) int { return cmp.Compare(x.ID, y.ID) })
	return m
}
