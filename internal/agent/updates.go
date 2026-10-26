package agent

import (
	"time"

	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
	"github.com/lsariol/sonar/internal/store"
)

const (
	// updateLimit is how long a service may be updating before the update
	// counts as timed out. Lighthouse gives a deploy the same 2 minutes to
	// become healthy before it rolls back (its deploy.Timeouts.Verify).
	updateLimit = 2 * time.Minute

	// forgetAfter is how long a service that's gone is remembered, so a
	// replacement that's slow to appear still counts as an update.
	forgetAfter = 10 * time.Minute
)

// Update outcomes.
const (
	updateOK       = "ok"
	updateFailed   = "failed"
	updateTimedOut = "timed out"
)

// seenService is what the agent knows of a service's containers: the settled
// ones (its baseline), and newcomers that appeared since.
type seenService struct {
	settled  map[string]string    // container ID → its com.mobasity.version
	newcomer map[string]time.Time // container ID → first seen
	at       time.Time            // last seen
}

// update is a service being replaced by a deploy.
type update struct {
	started  time.Time
	from, to string
	newIDs   map[string]bool
}

// trackUpdates notices deploys from the container list alone. Docker Compose
// creates the new container first (under a temporary name, beside the old
// one), then stops the old one, removes it and starts the new one. So: a
// settled container leaving (stopped or removed) while a newcomer exists is
// an update, which lasts until the newcomers are ready, or 2 minutes. A
// newcomer that comes up while every settled container keeps running is a
// replica being added. A container restarting in place keeps its ID, so a
// crash is never an update. Callers hold a.mu.
func (a *Agent) trackUpdates(now time.Time) {
	byService := make(map[serviceKey][]containers.Container)
	for _, c := range a.containers {
		// One-off jobs get new containers every run; only long-running
		// services are updated.
		if c.Project != "" && c.RestartPolicy != "no" && c.RestartPolicy != "" {
			k := serviceKey{c.Project, c.Service}
			byService[k] = append(byService[k], c)
		}
	}

	for k, cs := range byService {
		s, known := a.seen[k]
		if !known {
			s = seenService{settled: versions(cs), newcomer: map[string]time.Time{}}
		}
		s.at = now
		cur := make(map[string]containers.Container, len(cs))
		for _, c := range cs {
			cur[c.ID] = c
			if _, ok := s.settled[c.ID]; !ok {
				if _, ok := s.newcomer[c.ID]; !ok {
					s.newcomer[c.ID] = now
				}
			}
		}
		for id := range s.newcomer {
			if _, ok := cur[id]; !ok {
				delete(s.newcomer, id)
			}
		}

		if a.updates[k] == nil && len(s.newcomer) > 0 {
			if from, leaving := leavingVersion(s.settled, cur); leaving {
				a.updates[k] = newUpdate(now, from, s.newcomer, cur)
			} else {
				settleReplicas(&s, cur, now)
			}
		}
		a.seen[k] = s
	}
	for k, s := range a.seen {
		if _, ok := byService[k]; !ok && now.Sub(s.at) > forgetAfter {
			delete(a.seen, k)
		}
	}

	for k, u := range a.updates {
		outcome := updateProgress(byService[k], u, now)
		if outcome == "" {
			continue
		}
		b := a.bucketAt(now)
		b.updates = append(b.updates, store.Update{Project: k.project, Service: k.service,
			Started: u.started, Ended: now.UTC(), From: u.from, To: u.to, Outcome: outcome})
		if outcome != updateOK {
			a.opts.Log.Warn("an update didn't come up healthy", "project", k.project, "service", k.service,
				"to", u.to, "outcome", outcome)
		}
		delete(a.updates, k)
		if s, ok := a.seen[k]; ok {
			a.seen[k] = seenService{settled: versions(byService[k]), newcomer: map[string]time.Time{}, at: s.at}
		}
	}
}

// resume picks up what the previous agent saw from the snapshot it left, so a
// deploy that replaced the agent itself, or ran while it was away, is still
// an update.
func (a *Agent) resume() {
	snap, err := snapshot.Read(a.opts.StateDir)
	if err != nil {
		a.opts.Log.Info("starting without the previous agent's view", "reason", err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resumeFrom(snap, a.opts.Now())
}

// resumeFrom seeds each service's settled containers from snap, when it's at
// most 10 minutes old. A deploy's new containers (created but not started,
// or newer than an update in progress) are left out, so the next reading
// sees them as newcomers. Callers hold a.mu.
func (a *Agent) resumeFrom(snap snapshot.Snapshot, now time.Time) {
	if age := now.Sub(snap.GeneratedAt); age < 0 || age > forgetAfter {
		return
	}
	for _, p := range snap.Projects {
		if p.Project == "" {
			continue
		}
		for _, s := range p.Services {
			if !s.Counted {
				continue
			}
			settled := make(map[string]string)
			for _, c := range s.Containers {
				if c.State == "created" || (s.Update != nil && !c.CreatedAt.Before(s.Update.Since)) {
					continue
				}
				settled[c.ID] = c.Version
			}
			if len(settled) > 0 {
				a.seen[serviceKey{p.Project, s.Service}] = seenService{settled: settled, newcomer: map[string]time.Time{}, at: snap.GeneratedAt}
			}
		}
	}
}

func versions(cs []containers.Container) map[string]string {
	v := make(map[string]string, len(cs))
	for _, c := range cs {
		v[c.ID] = c.Version
	}
	return v
}

// leavingVersion reports whether a settled container is gone or no longer
// running, and its version.
func leavingVersion(settled map[string]string, cur map[string]containers.Container) (string, bool) {
	for id, version := range settled {
		if c, ok := cur[id]; !ok || c.State != "running" {
			return version, true
		}
	}
	return "", false
}

// newUpdate starts an update to the newcomers, dated from the earliest one's
// creation when Docker gives it.
func newUpdate(now time.Time, from string, newcomers map[string]time.Time, cur map[string]containers.Container) *update {
	u := &update{started: now.UTC(), from: from, newIDs: make(map[string]bool)}
	for id, first := range newcomers {
		u.newIDs[id] = true
		c := cur[id]
		u.to = c.Version
		start := first
		if !c.CreatedAt.IsZero() && now.Sub(c.CreatedAt) < updateLimit {
			start = c.CreatedAt
		}
		if start.Before(u.started) {
			u.started = start.UTC()
		}
	}
	return u
}

// settleReplicas moves newcomers that are up, beside settled containers that
// all still run, into the baseline: they were replicas being added.
func settleReplicas(s *seenService, cur map[string]containers.Container, now time.Time) {
	for id, first := range s.newcomer {
		c := cur[id]
		if st, _ := state.Container(c, now); st == state.Up || now.Sub(first) > updateLimit {
			s.settled[id] = c.Version
			delete(s.newcomer, id)
		}
	}
}

// updateProgress is "" while the update goes on: every counted container is
// up, or new and still getting ready, or an old one on its way out. It's the
// outcome once the update is over.
func updateProgress(cs []containers.Container, u *update, now time.Time) string {
	if now.Sub(u.started) > updateLimit {
		return updateTimedOut
	}
	pending := len(cs) == 0
	for _, c := range cs {
		st, counted := state.Container(c, now)
		switch {
		case !counted || st == state.Up:
		case !u.newIDs[c.ID] && c.State != "running":
			// An old container on its way out: Compose removes it once the new one runs.
		case st == state.Unknown:
			pending = true
		case u.newIDs[c.ID] && c.State == "running" && c.Health == "starting":
			pending = true
		default:
			return updateFailed
		}
	}
	if pending {
		return ""
	}
	return updateOK
}

// serviceState is a service's state at now: Updating while an update is in
// progress, otherwise what its containers say. Callers hold a.mu.
func (a *Agent) serviceState(k serviceKey, cs []containers.Container, now time.Time) (state.State, bool) {
	st, counted := state.Service(cs, now)
	if counted && a.updates[k] != nil {
		return state.Updating, true
	}
	return st, counted
}
