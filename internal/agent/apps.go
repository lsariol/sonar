package agent

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/report"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/store"
)

const (
	appsCollector = "apps"
	appsEvery     = time.Minute
)

// Reports is where the agent reads the apps' reports.
type Reports interface {
	Fetch(ctx context.Context, container string, port int) (report.Report, error)
}

// appState is what the agent knows of one container's report.
type appState struct {
	project, service, container string
	readAt                      time.Time
	err                         string
	startedAt                   time.Time
	counters                    map[string]uint64
	checks                      map[string]appCheck
}

type appCheck struct {
	ok      bool
	message string
	since   time.Time
}

// reporter is a running container with a report-port label.
type reporter struct {
	id, project, service, name string
	port                       int
}

// sampleApps reads every report and files counter growth and changed checks; a failed reading counts nothing.
func (a *Agent) sampleApps(ctx context.Context) {
	a.mu.Lock()
	var list []reporter
	var refused []string
	for _, c := range a.containers {
		if c.ReportPort == "" || c.State != "running" || c.Project == "" {
			continue
		}
		port, err := report.ParsePort(c.ReportPort)
		if err != nil {
			refused = append(refused, fmt.Sprintf("%s: label %s=%q %s", whose(c.Project, c.Service), containers.ReportPortLabel, c.ReportPort, err))
			continue
		}
		list = append(list, reporter{id: c.ID, project: c.Project, service: c.Service, name: c.Name, port: port})
	}
	a.mu.Unlock()

	reports := make([]report.Report, len(list))
	errs := make([]error, len(list))
	var wg sync.WaitGroup
	slots := make(chan struct{}, checkParallel)
	for i, r := range list {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			reports[i], errs[i] = a.opts.Reports.Fetch(ctx, r.name, r.port)
		}()
	}
	wg.Wait()

	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.opts.Now().UTC()
	b := a.bucketAt(now)
	kept := make(map[string]*appState, len(list))
	for i, r := range list {
		st := a.apps[r.id]
		if st == nil {
			st = &appState{project: r.project, service: r.service, container: r.name, checks: map[string]appCheck{}}
		}
		kept[r.id] = st
		if errs[i] != nil {
			st.err = errs[i].Error()
			continue
		}
		rep := reports[i]
		st.err, st.readAt = "", now
		a.addCounters(b, st, rep, now)
		a.addChecks(b, st, rep, now)
	}
	a.apps, a.appsRefused = kept, refused
	a.record(appsCollector, nil)
}

// addCounters files each counter's growth: a restart or a new counter counts in full, a first reading only after a deploy.
func (a *Agent) addCounters(b *bucket, st *appState, rep report.Report, now time.Time) {
	first := st.counters == nil
	fresh := first && now.Sub(rep.StartedAt) <= 2*appsEvery
	restarted := !first && !rep.StartedAt.Equal(st.startedAt)
	for name, v := range rep.Counters {
		prev, had := st.counters[name]
		var grown uint64
		switch {
		case first && !fresh:
		case first, restarted, !had, v < prev:
			grown = v
		default:
			grown = v - prev
		}
		if grown > 0 {
			b.appCounters[appCounterKey{st.project, name}] += grown
		}
	}
	st.startedAt, st.counters = rep.StartedAt, rep.Counters
	if st.counters == nil {
		st.counters = map[string]uint64{}
	}
}

// addChecks notes each check's state and files the ones that changed or are new; callers hold a.mu.
func (a *Agent) addChecks(b *bucket, st *appState, rep report.Report, now time.Time) {
	current := make(map[string]appCheck, len(rep.Checks))
	for _, c := range rep.Checks {
		prev, had := st.checks[c.Name]
		ac := appCheck{ok: c.OK, message: c.Message, since: now}
		if had && prev.ok == c.OK {
			ac.since = prev.since
		} else {
			b.appChecks = append(b.appChecks, store.AppCheckChange{Project: st.project, Service: st.service, Name: c.Name,
				At: now, OK: c.OK, Message: c.Message})
		}
		current[c.Name] = ac
	}
	st.checks = current
}

type appCounterKey struct{ project, name string }

// buildApps is every report as last read. Callers hold a.mu.
func (a *Agent) buildApps() []snapshot.AppReport {
	out := []snapshot.AppReport{}
	for id, st := range a.apps {
		r := snapshot.AppReport{ContainerID: id, Project: st.project, Service: st.service, Container: st.container,
			Error: st.err, StartedAt: st.startedAt, Counters: st.counters, Checks: []snapshot.AppCheck{}}
		if !st.readAt.IsZero() {
			at := st.readAt
			r.ReadAt = &at
		}
		for name, c := range st.checks {
			r.Checks = append(r.Checks, snapshot.AppCheck{Name: name, OK: c.ok, Message: c.message, Since: c.since})
		}
		slices.SortFunc(r.Checks, func(x, y snapshot.AppCheck) int { return cmp.Compare(x.Name, y.Name) })
		out = append(out, r)
	}
	slices.SortFunc(out, func(x, y snapshot.AppReport) int {
		return cmp.Or(cmp.Compare(x.Project, y.Project), cmp.Compare(x.Service, y.Service), cmp.Compare(x.Container, y.Container))
	})
	return out
}

// resumeApps takes up the previous agent's last reports, so counting goes on where it stopped; callers hold a.mu.
func (a *Agent) resumeApps(snap snapshot.Snapshot) {
	for _, r := range snap.Apps {
		if r.ReadAt == nil {
			continue
		}
		st := &appState{project: r.Project, service: r.Service, container: r.Container, readAt: *r.ReadAt,
			startedAt: r.StartedAt, counters: r.Counters, checks: map[string]appCheck{}}
		if st.counters == nil {
			st.counters = map[string]uint64{}
		}
		for _, c := range r.Checks {
			st.checks[c.Name] = appCheck{ok: c.OK, message: c.Message, since: c.Since}
		}
		a.apps[r.ContainerID] = st
	}
}
