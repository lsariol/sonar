package state

import (
	"encoding/json"
	"testing"

	"github.com/lsariol/sonar/internal/containers"
)

func TestContainer(t *testing.T) {
	c := func(state, health, policy string) containers.Container {
		return containers.Container{State: state, Health: health, RestartPolicy: policy}
	}
	cases := []struct {
		name    string
		c       containers.Container
		want    State
		counted bool
	}{
		{"running, healthy", c("running", "healthy", "unless-stopped"), Up, true},
		{"running, no healthcheck", c("running", "none", "always"), Up, true},
		{"running, starting", c("running", "starting", "unless-stopped"), Degraded, true},
		{"running, unhealthy", c("running", "unhealthy", "unless-stopped"), Down, true},
		{"restarting", c("restarting", "none", "unless-stopped"), Down, true},
		{"paused", c("paused", "healthy", "unless-stopped"), Down, true},
		{"exited", c("exited", "none", "on-failure"), Down, true},
		{"dead", c("dead", "none", "always"), Down, true},
		{"job, exited 0", c("exited", "none", "no"), Unknown, false},
		{"job, running", c("running", "none", "no"), Unknown, false},
	}
	for _, tc := range cases {
		got, counted := Container(tc.c)
		if got != tc.want || counted != tc.counted {
			t.Errorf("%s: %v, %v; want %v, %v", tc.name, got, counted, tc.want, tc.counted)
		}
	}
}

func TestService(t *testing.T) {
	up := containers.Container{State: "running", Health: "healthy", RestartPolicy: "always"}
	down := containers.Container{State: "exited", RestartPolicy: "always"}
	job := containers.Container{State: "exited", RestartPolicy: "no"}

	if s, ok := Service([]containers.Container{up, down}); s != Down || !ok {
		t.Errorf("one replica down: %v, %v; want down", s, ok)
	}
	if s, ok := Service([]containers.Container{up, job}); s != Up || !ok {
		t.Errorf("a job beside it: %v, %v; want up", s, ok)
	}
	if _, ok := Service([]containers.Container{job}); ok {
		t.Error("only jobs: want not counted")
	}
}

func TestCheck(t *testing.T) {
	cases := []struct {
		prev     State
		failures int
		slow     bool
		want     State
	}{
		{Unknown, 0, false, Up},
		{Down, 0, false, Up},
		{Up, 0, true, Degraded},
		{Up, 1, false, Up},
		{Degraded, 1, false, Degraded},
		{Unknown, 1, false, Unknown},
		{Up, 2, false, Down},
		{Up, 7, false, Down},
	}
	for _, tc := range cases {
		if got := Check(tc.prev, tc.failures, tc.slow); got != tc.want {
			t.Errorf("Check(%v, %d, %v) = %v, want %v", tc.prev, tc.failures, tc.slow, got, tc.want)
		}
	}
}

func TestProject(t *testing.T) {
	cases := []struct {
		name     string
		services []State
		checks   []State
		want     State
		evidence Evidence
	}{
		{"check down beats healthy services", []State{Up}, []State{Down}, Down, Checked},
		{"check slow", []State{Up}, []State{Degraded}, Degraded, Checked},
		{"check fine, a service down", []State{Up, Down}, []State{Up}, Degraded, Checked},
		{"check fine, all up", []State{Up, Up}, []State{Up}, Up, Checked},
		{"two checks, one down", []State{Up}, []State{Up, Down}, Down, Checked},
		{"check without a result yet", []State{Up}, []State{Unknown}, Up, ContainerOnly},
		{"no checks, all down", []State{Down, Down}, nil, Down, ContainerOnly},
		{"no checks, some down", []State{Up, Down}, nil, Degraded, ContainerOnly},
		{"no checks, starting", []State{Up, Degraded}, nil, Degraded, ContainerOnly},
		{"no checks, all up", []State{Up}, nil, Up, ContainerOnly},
		{"nothing counted", nil, nil, Unknown, ContainerOnly},
	}
	for _, tc := range cases {
		got, ev := Project(tc.services, tc.checks)
		if got != tc.want || ev != tc.evidence {
			t.Errorf("%s: %v, %s; want %v, %s", tc.name, got, ev, tc.want, tc.evidence)
		}
	}
}

func TestDay(t *testing.T) {
	cases := []struct {
		up, degraded, down, window int
		want                       State
		ok                         bool
	}{
		{1440, 0, 0, 1440, Up, true},
		{1430, 6, 4, 1440, Degraded, true}, // 4 down minutes aren't enough for down
		{1430, 0, 10, 1440, Down, true},
		{1436, 2, 2, 1440, Up, true}, // 4 bad minutes in total: a blip
		{1436, 2, 3, 1440, Degraded, true},
		{700, 0, 0, 1440, Unknown, false}, // under half the day observed
		{720, 0, 0, 1440, Up, true},
		{300, 0, 0, 600, Up, true}, // today, 10 hours in
		{0, 0, 0, 0, Unknown, false},
	}
	for _, tc := range cases {
		got, ok := Day(tc.up, tc.degraded, tc.down, tc.window)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Day(%d, %d, %d, %d) = %v, %v; want %v, %v", tc.up, tc.degraded, tc.down, tc.window, got, ok, tc.want, tc.ok)
		}
	}
}

func TestUptime(t *testing.T) {
	if pct, ok := Uptime(1438, 1, 1); !ok || pct < 99.93 || pct > 99.94 {
		t.Errorf("Uptime = %v, %v; want 99.93", pct, ok)
	}
	if _, ok := Uptime(0, 0, 0); ok {
		t.Error("nothing observed: want ok false")
	}
}

func TestStateJSON(t *testing.T) {
	data, err := json.Marshal(map[string]State{"a": Degraded})
	if err != nil || string(data) != `{"a":"degraded"}` {
		t.Fatalf("Marshal = %s, %v", data, err)
	}
	var back map[string]State
	if err := json.Unmarshal(data, &back); err != nil || back["a"] != Degraded {
		t.Errorf("Unmarshal = %v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"a":"sideways"}`), &back); err == nil {
		t.Error("unknown state: want an error")
	}
	if Worst() != Unknown || Worst(Unknown, Up) != Up || Worst(Up, Down, Degraded) != Down {
		t.Error("Worst ordering is wrong")
	}
}
