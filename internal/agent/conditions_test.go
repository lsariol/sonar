package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
)

func TestConditions(t *testing.T) {
	ok := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	healthy := snapshot.Snapshot{
		Collectors: map[string]snapshot.Collector{"host": {State: snapshot.Available}, "docker": {State: snapshot.Available}},
		Host: &snapshot.Host{MemoryTotal: 16e9, MemoryUsed: 8e9, MemoryUsedPercent: 50, SwapTotal: 4e9, SwapUsed: 1e9,
			Load15: 3, CorePercents: []float64{1, 2, 3, 4},
			Disk: &snapshot.Disk{Total: 450e9, Used: 200e9, Available: 250e9, UsedPercent: 44, Inodes: 1000, InodesFree: 900}},
		Projects: []snapshot.Project{
			{Project: "sparkdb", Group: "infrastructure", State: state.Up},
			{Project: "plop", Group: "app", State: state.Down}, // an app down isn't Spark's condition
			{Project: "lys", State: state.Down},                // no group: nothing to judge by
		},
	}
	if got := conditions(healthy); len(got) != 0 {
		t.Errorf("a healthy Spark has conditions: %+v", got)
	}

	sick := healthy
	sick.Collectors = map[string]snapshot.Collector{
		"docker":   {State: snapshot.Unavailable, Error: "proxy: connection refused"},
		"database": {State: snapshot.Stale, LastOK: &ok},
		"network":  {State: snapshot.Unsupported, Error: "no host /proc"}, // by design, not a problem
	}
	sick.Host = &snapshot.Host{MemoryTotal: 16e9, MemoryUsed: 15.5e9, MemoryUsedPercent: 96.9, SwapTotal: 4e9, SwapUsed: 2.4e9,
		Load15: 9, CorePercents: []float64{1, 2, 3, 4},
		Disk: &snapshot.Disk{Total: 450e9, Used: 400e9, Available: 50e9, UsedPercent: 88.9, Inodes: 1000, InodesFree: 40}}
	sick.Projects = []snapshot.Project{
		{Project: "sparkdb", Group: "infrastructure", State: state.Down},
		{Project: "cove", Group: "infrastructure", State: state.Degraded},
	}
	got := conditions(sick)
	want := []string{
		"collector/docker critical", "inodes/ critical", "memory/ critical", "project/sparkdb critical",
		"collector/database warning", "disk/ warning", "load/ warning", "project/cove warning", "swap/ warning",
	}
	var have []string
	for _, c := range got {
		have = append(have, c.ID+"/"+c.Subject+" "+c.Level)
	}
	if !slices.Equal(have, want) {
		t.Errorf("conditions =\n%v\nwant\n%v", have, want)
	}
	for _, c := range got {
		if c.ID == "disk" && c.Message != "The disk is 89% full: 50.0 GB free of 450.0 GB" {
			t.Errorf("disk message = %q", c.Message)
		}
		if c.ID == "project" && c.Subject == "sparkdb" && c.Message != "sparkdb (infrastructure) is down" {
			t.Errorf("sparkdb message = %q", c.Message)
		}
	}
}

// TestConditionSince: a condition keeps the time it began while it lasts,
// across a change of level and a restart of the agent; once gone, it starts
// over.
func TestConditionSince(t *testing.T) {
	cove := running('a', "cove", "cove", "")
	cove.Group = "infrastructure"
	cove.Health = "starting" // degraded
	d := &fakeDocker{list: []containers.Container{cove}}
	a, clk := newTestAgent(t, d, &fakeChecker{})
	a.sampleDocker(context.Background())
	began := clk.t
	first := a.build()
	if len(first.Conditions) != 1 || first.Conditions[0].Level != "warning" || !first.Conditions[0].Since.Equal(began) {
		t.Fatalf("conditions = %+v, want cove degraded since now", first.Conditions)
	}

	clk.t = clk.t.Add(time.Minute)
	cove.Health = "unhealthy" // down: worse, but the same condition
	d.list = []containers.Container{cove}
	a.sampleDocker(context.Background())
	s := a.build()
	if len(s.Conditions) != 1 || s.Conditions[0].Level != "critical" || !s.Conditions[0].Since.Equal(began) {
		t.Errorf("after a minute, down: %+v, want critical since %s", s.Conditions, began)
	}

	// A new agent picks the start time up from the snapshot.
	b, _ := newTestAgent(t, d, &fakeChecker{})
	b.opts.Now = func() time.Time { return clk.t.Add(time.Minute) }
	b.resumeFrom(s, clk.t)
	b.sampleDocker(context.Background())
	if r := b.build(); len(r.Conditions) != 1 || !r.Conditions[0].Since.Equal(began) {
		t.Errorf("after a restart: %+v, want since %s", r.Conditions, began)
	}

	cove.Health = "healthy"
	d.list = []containers.Container{cove}
	a.sampleDocker(context.Background())
	if s := a.build(); len(s.Conditions) != 0 || len(a.conditionSince) != 0 {
		t.Errorf("recovered: %+v", s.Conditions)
	}
	if strings.Contains(first.Conditions[0].Message, "%") {
		t.Errorf("message = %q", first.Conditions[0].Message)
	}
}
