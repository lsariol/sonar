package agent

import (
	"context"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/checks"
	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/state"
)

// deploy steps the agent through readings 10 s apart, each with list as the
// containers, and returns the project's state after each.
type deploy struct {
	t   *testing.T
	a   *Agent
	clk *clock
	d   *fakeDocker
}

func newDeploy(t *testing.T, c *fakeChecker) *deploy {
	d := &fakeDocker{}
	a, clk := newTestAgent(t, d, c)
	return &deploy{t: t, a: a, clk: clk, d: d}
}

func (dp *deploy) reading(list ...containers.Container) state.State {
	dp.t.Helper()
	dp.d.list = list
	dp.clk.t = dp.clk.t.Add(dockerEvery)
	dp.a.sampleDocker(context.Background())
	s := dp.a.build()
	for _, p := range s.Projects {
		if p.Project == "plop" {
			return p.State
		}
	}
	return state.Unknown
}

func (dp *deploy) container(id byte, version, st, health string, createdAgo time.Duration) containers.Container {
	c := running(id, "plop", "plop", "")
	c.Version, c.State, c.Health = version, st, health
	c.CreatedAt = dp.clk.t.Add(dockerEvery - createdAgo)
	return c
}

func (dp *deploy) stopped(id byte, version string, ago time.Duration) containers.Container {
	c := running(id, "plop", "plop", "")
	c.Version, c.State = version, "exited"
	at := dp.clk.t.Add(dockerEvery - ago)
	c.FinishedAt = &at
	return c
}

func (dp *deploy) finished() []string {
	var out []string
	for _, b := range dp.a.buckets {
		for _, u := range b.updates {
			out = append(out, u.From+"→"+u.To+" "+u.Outcome)
		}
	}
	return out
}

func TestDeploy(t *testing.T) {
	dp := newDeploy(t, &fakeChecker{})
	old := dp.container('a', "v1.4.0", "running", "healthy", time.Hour)
	if s := dp.reading(old); s != state.Up {
		t.Fatalf("before: %v", s)
	}

	steps := []struct {
		name string
		list []containers.Container
		want state.State
	}{
		{"old stopped a second ago, no replacement yet", []containers.Container{dp.stopped('a', "v1.4.0", time.Second)}, state.Unknown},
		{"new one created", []containers.Container{dp.container('b', "v1.5.0", "created", "none", 2*time.Second)}, state.Updating},
		{"new one starting", []containers.Container{dp.container('b', "v1.5.0", "running", "starting", 12*time.Second)}, state.Updating},
		{"new one healthy", []containers.Container{dp.container('b', "v1.5.0", "running", "healthy", 22*time.Second)}, state.Up},
	}
	for _, s := range steps {
		if got := dp.reading(s.list...); got != s.want {
			t.Errorf("%s: %v, want %v", s.name, got, s.want)
		}
	}
	if got := dp.finished(); len(got) != 1 || got[0] != "v1.4.0→v1.5.0 ok" {
		t.Errorf("recorded updates = %v", got)
	}
	if len(dp.a.updates) != 0 {
		t.Error("the update should be over")
	}
}

// TestComposeRecreate is exactly what `docker compose up` did on Docker
// Desktop (recorded through the proxy): it creates the new container first,
// under a temporary name, while the old one still runs; then stops the old
// one; then removes it and starts the new one.
func TestComposeRecreate(t *testing.T) {
	dp := newDeploy(t, &fakeChecker{})
	old := dp.container('a', "v4", "running", "healthy", time.Hour)
	dp.reading(old)

	// Each step builds its containers at its own reading, so their ages are
	// right: the new container was created 1 s before the first of them.
	steps := []struct {
		name string
		list func() []containers.Container
		want state.State
	}{
		{"new created beside the running old one", func() []containers.Container {
			return []containers.Container{dp.container('b', "v5", "created", "none", time.Second), old}
		}, state.Up},
		{"old stopped, new still created", func() []containers.Container {
			return []containers.Container{dp.container('b', "v5", "created", "none", 11*time.Second), dp.stopped('a', "v4", 2*time.Second)}
		}, state.Updating},
		{"old removed, new starting", func() []containers.Container {
			return []containers.Container{dp.container('b', "v5", "running", "starting", 21*time.Second)}
		}, state.Updating},
		{"new healthy", func() []containers.Container {
			return []containers.Container{dp.container('b', "v5", "running", "healthy", 31*time.Second)}
		}, state.Up},
	}
	for _, s := range steps {
		if got := dp.reading(s.list()...); got != s.want {
			t.Errorf("%s: %v, want %v", s.name, got, s.want)
		}
	}
	if got := dp.finished(); len(got) != 1 || got[0] != "v4→v5 ok" {
		t.Errorf("recorded updates = %v", got)
	}
}

func TestDeployShowsInSnapshot(t *testing.T) {
	dp := newDeploy(t, &fakeChecker{})
	dp.reading(dp.container('a', "v1.4.0", "running", "healthy", time.Hour))
	dp.reading(dp.container('b', "v1.5.0", "running", "starting", 3*time.Second))
	svc := dp.a.build().Projects[0].Services[0]
	if svc.State != state.Updating || svc.Update == nil || svc.Update.From != "v1.4.0" || svc.Update.To != "v1.5.0" {
		t.Errorf("service = %+v, update %+v", svc, svc.Update)
	}
	if b := dp.a.buckets[dp.clk.t.Truncate(time.Minute)]; b == nil || b.services[serviceKey{"plop", "plop"}].state != state.Updating {
		t.Error("the minute should record the service as updating")
	}
}

// TestDeployWhileCheckFails: visitors may see errors during the swap; within
// the 2 minutes it's an update, not an outage (option A).
func TestDeployWhileCheckFails(t *testing.T) {
	url := "https://plop.mobasity.com"
	c := &fakeChecker{queue: map[string][]checks.Outcome{url: {checks.OK, checks.Failed, checks.Failed}}}
	dp := newDeploy(t, c)
	old := dp.container('a', "v1.4.0", "running", "healthy", time.Hour)
	old.PublicURL = url
	dp.reading(old)
	dp.a.runChecks(context.Background())

	fresh := dp.container('b', "v1.5.0", "running", "starting", 3*time.Second)
	fresh.PublicURL = url
	dp.reading(fresh)
	dp.a.runChecks(context.Background())
	dp.a.runChecks(context.Background())
	if p := dp.a.build().Projects[0]; p.State != state.Updating || p.Evidence != state.Checked {
		t.Errorf("check down twice during the update: %v %s, want updating", p.State, p.Evidence)
	}
}

func TestFailedDeploy(t *testing.T) {
	dp := newDeploy(t, &fakeChecker{})
	dp.reading(dp.container('a', "v1.4.0", "running", "healthy", time.Hour))
	dp.reading(dp.container('b', "v1.5.0", "running", "starting", 3*time.Second))
	if s := dp.reading(dp.container('b', "v1.5.0", "running", "unhealthy", 13*time.Second)); s != state.Down {
		t.Errorf("the new version turned unhealthy: %v, want down", s)
	}
	if got := dp.finished(); len(got) != 1 || got[0] != "v1.4.0→v1.5.0 failed" {
		t.Errorf("recorded updates = %v", got)
	}

	// Lighthouse rolls back: another replacement, recorded as its own update.
	dp.reading(dp.container('c', "v1.4.0", "running", "starting", 2*time.Second))
	dp.reading(dp.container('c', "v1.4.0", "running", "healthy", 12*time.Second))
	if got := dp.finished(); len(got) != 2 || got[1] != "v1.5.0→v1.4.0 ok" {
		t.Errorf("after the rollback: %v", got)
	}
}

func TestDeployTimesOut(t *testing.T) {
	dp := newDeploy(t, &fakeChecker{})
	dp.reading(dp.container('a', "v1.4.0", "running", "healthy", time.Hour))
	for n := range 13 {
		dp.reading(dp.container('b', "v1.5.0", "running", "starting", time.Duration(n)*dockerEvery))
	}
	if s := dp.reading(dp.container('b', "v1.5.0", "running", "starting", 130*time.Second)); s != state.Degraded {
		t.Errorf("still starting after 2 minutes: %v, want degraded (no longer an update)", s)
	}
	if got := dp.finished(); len(got) != 1 || got[0] != "v1.4.0→v1.5.0 timed out" {
		t.Errorf("recorded updates = %v", got)
	}
}

func TestCrashIsNotAnUpdate(t *testing.T) {
	dp := newDeploy(t, &fakeChecker{})
	c := dp.container('a', "v1.4.0", "running", "healthy", time.Hour)
	dp.reading(c)
	c.State, c.RestartCount = "restarting", 1
	if s := dp.reading(c); s != state.Down {
		t.Errorf("a crash, restarting in place: %v, want down", s)
	}
	if len(dp.a.updates) != 0 || len(dp.finished()) != 0 {
		t.Error("a restart in place isn't an update")
	}

	crashed := dp.stopped('a', "v1.4.0", 30*time.Second)
	if s := dp.reading(crashed); s != state.Down {
		t.Errorf("stopped 30 s ago, nothing replacing it: %v, want down", s)
	}
}

func TestJobRunsAreNotUpdates(t *testing.T) {
	dp := newDeploy(t, &fakeChecker{})
	job := func(id byte) containers.Container {
		c := dp.container(id, "v1", "exited", "none", time.Minute)
		c.Service, c.RestartPolicy = "migrate", "no"
		return c
	}
	web := dp.container('w', "v1", "running", "healthy", time.Hour)
	dp.reading(web, job('a'))
	dp.reading(web, job('b'))
	if len(dp.a.updates) != 0 || len(dp.finished()) != 0 {
		t.Error("a one-off job's new container each run isn't an update")
	}
}

func TestScaleUpIsNotAnUpdate(t *testing.T) {
	dp := newDeploy(t, &fakeChecker{})
	one := dp.container('a', "v1.4.0", "running", "healthy", time.Hour)
	dp.reading(one)
	two := dp.container('b', "v1.4.0", "running", "starting", 3*time.Second)
	if s := dp.reading(one, two); s != state.Degraded {
		t.Errorf("a new replica starting beside the old one: %v, want degraded", s)
	}
	if len(dp.a.updates) != 0 {
		t.Error("adding a replica isn't an update")
	}
}

// TestSwapBetweenReadings: a quick deploy can finish within one 10-second
// gap, so the old container is gone and the new one running by the next
// reading. It's still an update.
func TestSwapBetweenReadings(t *testing.T) {
	dp := newDeploy(t, &fakeChecker{})
	dp.reading(dp.container('a', "v0.1.0", "running", "healthy", time.Hour))
	if s := dp.reading(dp.container('b', "v0.1.1", "running", "healthy", 4*time.Second)); s != state.Up {
		t.Errorf("after the swap: %v, want up", s)
	}
	if got := dp.finished(); len(got) != 1 || got[0] != "v0.1.0→v0.1.1 ok" {
		t.Errorf("recorded updates = %v, want one", got)
	}

	dp = newDeploy(t, &fakeChecker{})
	dp.reading(dp.container('a', "v0.1.0", "running", "healthy", time.Hour))
	if s := dp.reading(dp.container('b', "v0.1.1", "running", "starting", 4*time.Second)); s != state.Updating {
		t.Errorf("new one still starting: %v, want updating", s)
	}
	dp.reading(dp.container('b', "v0.1.1", "running", "healthy", 14*time.Second))
	if got := dp.finished(); len(got) != 1 || got[0] != "v0.1.0→v0.1.1 ok" {
		t.Errorf("recorded updates = %v, want one", got)
	}
}
