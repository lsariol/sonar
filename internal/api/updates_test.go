package api

import (
	"encoding/json"
	"testing"
	"time"

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
