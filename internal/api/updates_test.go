package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
	"github.com/lsariol/sonar/internal/store"
)

func TestUpdatingIsPublic(t *testing.T) {
	snap := testSnapshot(now)
	for i := range snap.Projects {
		if snap.Projects[i].Project == "plop" {
			snap.Projects[i].State = state.Updating
		}
	}
	s := newTestServer(t, &fakeReader{days: testDays()}, &fakeSnapshots{snap: snap})
	_, _, body := do(t, s, "GET", "/v1/status?days=1", "Bearer "+token)
	var a statusAnswer
	json.Unmarshal(body, &a)
	states := map[string]string{}
	for _, p := range a.Projects {
		states[p.Project] = p.State
	}
	if states["plop"] != "updating" || a.Overall.State != "down" {
		t.Errorf("plop %q, overall %q; want updating, and down overall (lys is down)", states["plop"], a.Overall.State)
	}
}

func TestMergeUpdates(t *testing.T) {
	at := now.Add(-time.Hour)
	u := func(project, service string, startAfter, took time.Duration, outcome string) store.Update {
		return store.Update{Project: project, Service: service, Started: at.Add(startAfter), Ended: at.Add(startAfter + took), Outcome: outcome}
	}
	got := mergeUpdates([]store.Update{
		u("plop", "web", 0, 30*time.Second, "ok"),
		u("plop", "worker", 50*time.Second, 10*time.Second, "failed"), // 20 s after web ended: same deploy
		u("plop", "web", 10*time.Minute, 20*time.Second, "ok"),        // a later deploy
		u("lys", "lys", 5*time.Second, 15*time.Second, "ok"),
		u("cloudflared", "cloudflared", 0, 5*time.Second, "ok"), // excluded
	})
	if len(got) != 3 {
		t.Fatalf("merged = %+v, want 3", got)
	}
	first, lys, later := got[0], got[1], got[2]
	if first.Project != "plop" || first.Seconds != 60 || first.Outcome != "failed" {
		t.Errorf("plop's first deploy = %+v, want 60 s, failed (one service failed)", first)
	}
	if lys.Project != "lys" || later.Project != "plop" || later.Seconds != 20 || later.Outcome != "ok" {
		t.Errorf("others = %+v %+v", lys, later)
	}

	// Back-to-back deploys of one service (seen live): a failed one, then its
	// rollback 10 s later, stay two markers.
	back := mergeUpdates([]store.Update{
		u("plop", "web", 0, 40*time.Second, "failed"),
		u("plop", "web", 50*time.Second, 20*time.Second, "ok"),
	})
	if len(back) != 2 || back[0].Outcome != "failed" || back[1].Outcome != "ok" {
		t.Errorf("a deploy and its rollback = %+v, want two markers", back)
	}
}

func TestPublicVersion(t *testing.T) {
	for v, ok := range map[string]bool{
		"v1.3.0": true, "1.3.0": true, "v1.3.0-rc.1": true, "v10.20.300": true,
		"": false, "dev": false, "a1b2c3d": false, "v1.3": false, "v01.3.0": false, "v1.3.0 ": false,
		"v1.3.0\n": false, "v1.3.0+build": false, "v1.3.0-" + strings.Repeat("x", 60): false,
	} {
		if got := publicVersion(v); (got != nil) != ok {
			t.Errorf("publicVersion(%q) = %v, want ok %v", v, got, ok)
		}
	}
}

func TestProjectVersion(t *testing.T) {
	svc := func(counted bool, versions ...string) snapshot.Service {
		s := snapshot.Service{Counted: counted}
		for _, v := range versions {
			s.Containers = append(s.Containers, snapshot.Container{Version: v})
		}
		return s
	}
	updating := svc(true, "v1.2.0", "v1.3.0")
	updating.Update = &snapshot.ServiceUpdate{From: "v1.2.0", To: "v1.3.0"}
	cases := []struct {
		name     string
		services []snapshot.Service
		want     string // "" for nil
	}{
		{"one service", []snapshot.Service{svc(true, "v1.3.0")}, "v1.3.0"},
		{"replicas and services agree", []snapshot.Service{svc(true, "v1.3.0", "v1.3.0"), svc(true, "v1.3.0")}, "v1.3.0"},
		{"a job's old version is ignored", []snapshot.Service{svc(true, "v1.3.0"), svc(false, "v1.1.0")}, "v1.3.0"},
		{"mid-deploy: the version being replaced", []snapshot.Service{updating, svc(true, "v1.2.0")}, "v1.2.0"},
		{"services disagree", []snapshot.Service{svc(true, "v1.3.0"), svc(true, "v1.2.0")}, ""},
		{"a container without a label", []snapshot.Service{svc(true, "v1.3.0"), svc(true, "")}, ""},
		{"a commit, not a release", []snapshot.Service{svc(true, "a1b2c3d")}, ""},
		{"a dev build", []snapshot.Service{svc(true, "dev")}, ""},
		{"no containers", []snapshot.Service{svc(true)}, ""},
		{"only jobs", []snapshot.Service{svc(false, "v1.3.0")}, ""},
	}
	for _, tc := range cases {
		got := projectVersion(snapshot.Project{Services: tc.services})
		if (got == nil && tc.want != "") || (got != nil && *got != tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, deref(got), tc.want)
		}
	}
}

func TestMergeUpdateVersions(t *testing.T) {
	at := now.Add(-time.Hour)
	u := func(service string, after time.Duration, from, to string) store.Update {
		return store.Update{Project: "plop", Service: service, Started: at.Add(after), Ended: at.Add(after + 20*time.Second),
			From: from, To: to, Outcome: "ok"}
	}
	got := mergeUpdates([]store.Update{
		u("web", 0, "v1.2.0", "v1.3.0"),
		u("worker", 5*time.Second, "v1.2.0", "v1.3.0"), // same deploy, same versions
		u("web", 10*time.Minute, "v1.3.0", "v1.4.0"),
		u("worker", 10*time.Minute, "v1.2.9", "v1.4.0"), // same deploy, came from another version
		u("web", 20*time.Minute, "a1b2c3d", "v1.4.1"),   // from a commit deploy
	})
	want := [][2]string{{"v1.2.0", "v1.3.0"}, {"", "v1.4.0"}, {"", "v1.4.1"}}
	if len(got) != len(want) {
		t.Fatalf("merged = %+v, want %d markers", got, len(want))
	}
	for i, w := range want {
		if deref(got[i].From) != w[0] || deref(got[i].To) != w[1] {
			t.Errorf("marker %d: %q → %q, want %q → %q", i, deref(got[i].From), deref(got[i].To), w[0], w[1])
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
