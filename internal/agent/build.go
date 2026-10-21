package agent

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
)

// build assembles the snapshot from the latest readings. Callers hold a.mu.
func (a *Agent) build() snapshot.Snapshot {
	now := a.opts.Now().UTC()
	s := snapshot.Snapshot{
		Format:      snapshot.Format,
		GeneratedAt: now,
		Version:     a.opts.Version,
		Collectors:  make(map[string]snapshot.Collector, len(a.collectors)),
		Projects:    []snapshot.Project{},
		Checks:      []snapshot.Check{},
		Attention:   []string{},
	}

	names := make([]string, 0, len(a.collectors))
	for name := range a.collectors {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		st := a.collectors[name].status(now)
		s.Collectors[name] = st
		switch st.State {
		case snapshot.Unavailable:
			s.Attention = append(s.Attention, name+": "+st.Error)
		case snapshot.Stale:
			s.Attention = append(s.Attention, fmt.Sprintf("%s: no reading since %s", name, st.LastOK.Format(time.RFC3339)))
		}
	}

	s.Host = a.buildHost()

	checksByProject := make(map[string][]state.State)
	for _, t := range a.targets {
		c := snapshot.Check{Project: t.project, Service: t.service, URL: t.url}
		if st := a.checks[t]; st != nil {
			c.State, c.ConsecutiveFailures = st.state, st.failures
			last := st.last
			at := last.At
			c.LastAt, c.LastOutcome, c.LastStatus, c.LastError = &at, string(last.Outcome), last.Status, last.Error
			if last.Latency > 0 {
				ms := last.Latency.Milliseconds()
				c.LastLatencyMs = &ms
			}
			if last.Refused {
				s.Attention = append(s.Attention, fmt.Sprintf("%s: %s: %s", whose(t.project, t.service), t.url, last.Error))
			}
		}
		s.Checks = append(s.Checks, c)
		checksByProject[t.project] = append(checksByProject[t.project], c.State)
	}
	s.Attention = append(s.Attention, a.refused...)

	dockerLastOK := a.collectors[dockerCollector].lastOK
	dockerFresh := !dockerLastOK.IsZero() && now.Sub(dockerLastOK) <= dockerFreshFor
	s.Projects = a.buildProjects(checksByProject, dockerFresh)
	return s
}

func (a *Agent) buildHost() *snapshot.Host {
	if a.hostAt.IsZero() {
		return nil
	}
	h := &snapshot.Host{
		ReadAt:            a.hostAt.UTC(),
		CPUPercent:        a.cpuPct,
		CorePercents:      a.corePcts,
		MemoryTotal:       a.memory.Total,
		MemoryUsed:        a.memory.Used(),
		MemoryUsedPercent: a.memory.UsedPercent(),
		SwapTotal:         a.memory.SwapTotal,
		SwapUsed:          a.memory.SwapUsed(),
		Load1:             a.load.One,
		Load5:             a.load.Five,
		Load15:            a.load.Fifteen,
		UptimeSeconds:     a.uptime.Seconds(),
	}
	if a.disk != nil {
		d := a.disk
		h.Disk = &snapshot.Disk{Total: d.Total, Used: d.Used, Available: d.Available, UsedPercent: d.UsedPercent(),
			Inodes: d.Inodes, InodesFree: d.InodesFree}
	}
	return h
}

// buildProjects groups containers by compose project and service and
// decides each one's state. Without fresh Docker data, every project is
// unknown.
func (a *Agent) buildProjects(checksByProject map[string][]state.State, dockerFresh bool) []snapshot.Project {
	byProject := make(map[string]map[string][]containers.Container)
	for _, c := range a.containers {
		if byProject[c.Project] == nil {
			byProject[c.Project] = make(map[string][]containers.Container)
		}
		byProject[c.Project][c.Service] = append(byProject[c.Project][c.Service], c)
	}

	projects := make([]snapshot.Project, 0, len(byProject))
	for name, services := range byProject {
		p := snapshot.Project{Project: name, Services: []snapshot.Service{}}
		var counted []state.State
		for svcName, cs := range services {
			svcState, ok := state.Service(cs)
			svc := snapshot.Service{Service: svcName, State: svcState, Counted: ok, Containers: []snapshot.Container{}}
			var usages []snapshot.Usage
			for _, c := range cs {
				u := a.usage[c.ID]
				usages = append(usages, u)
				svc.Containers = append(svc.Containers, snapshot.Container{
					ID: c.ID, Name: c.Name, Image: c.Image, Version: c.Version, State: c.State, Health: c.Health,
					ExitCode: c.ExitCode, RestartCount: c.RestartCount, CreatedAt: c.CreatedAt,
					StartedAt: c.StartedAt, FinishedAt: c.FinishedAt, CPULimit: c.CPULimit, MemoryLimit: c.MemoryLimit,
					Usage: u,
				})
			}
			svc.Usage = sumUsage(usages)
			if !dockerFresh {
				svc.State = state.Unknown
			}
			if ok {
				counted = append(counted, svc.State)
			}
			p.Services = append(p.Services, svc)
		}
		slices.SortFunc(p.Services, func(x, y snapshot.Service) int { return cmp.Compare(x.Service, y.Service) })

		p.State, p.Evidence = state.Project(counted, checksByProject[name])
		if !dockerFresh {
			p.State = state.Unknown
		}
		projects = append(projects, p)
	}
	slices.SortFunc(projects, func(x, y snapshot.Project) int { return cmp.Compare(x.Project, y.Project) })
	return projects
}

// sumUsage adds up a service's containers. Each figure is nil when no
// container has it.
func sumUsage(us []snapshot.Usage) snapshot.Usage {
	var total snapshot.Usage
	for _, u := range us {
		total.CPUCores = addFloat(total.CPUCores, u.CPUCores)
		total.NetRxRate = addFloat(total.NetRxRate, u.NetRxRate)
		total.NetTxRate = addFloat(total.NetTxRate, u.NetTxRate)
		total.BlkReadRate = addFloat(total.BlkReadRate, u.BlkReadRate)
		total.BlkWriteRate = addFloat(total.BlkWriteRate, u.BlkWriteRate)
		if u.MemoryBytes != nil {
			m := *u.MemoryBytes
			if total.MemoryBytes != nil {
				m += *total.MemoryBytes
			}
			total.MemoryBytes = &m
		}
	}
	return total
}

func addFloat(sum, v *float64) *float64 {
	if v == nil {
		return sum
	}
	n := *v
	if sum != nil {
		n += *sum
	}
	return &n
}

// status is how a collector is doing at now.
func (c *collector) status(now time.Time) snapshot.Collector {
	st := snapshot.Collector{}
	if !c.lastOK.IsZero() {
		t := c.lastOK.UTC()
		st.LastOK = &t
	}
	switch {
	case c.err != nil && errors.Is(c.err, errors.ErrUnsupported):
		st.State, st.Error = snapshot.Unsupported, c.err.Error()
	case c.err != nil:
		st.State, st.Error = snapshot.Unavailable, c.err.Error()
	case c.lastOK.IsZero():
		st.State, st.Error = snapshot.Unavailable, "no reading yet"
	case now.Sub(c.lastOK) > 3*c.every:
		st.State = snapshot.Stale
	default:
		st.State = snapshot.Available
	}
	return st
}
