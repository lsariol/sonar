package state

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/containers"
)

var now = time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)

func TestContainer(t *testing.T) {
	c := func(state, health, policy string) containers.Container {
		return containers.Container{State: state, Health: health, RestartPolicy: policy}
	}
	stopped := func(ago time.Duration) containers.Container {
		at := now.Add(-ago)
		return containers.Container{State: "exited", RestartPolicy: "unless-stopped", FinishedAt: &at}
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
		{"created, never started", c("created", "none", "unless-stopped"), Down, true},
		{"created a second ago (a deploy's new container)", func() containers.Container {
			x := c("created", "none", "unless-stopped")
			x.CreatedAt = now.Add(-time.Second)
			return x
		}(), Unknown, true},
		{"exited, no finish time", c("exited", "none", "on-failure"), Down, true},
		{"stopped 5 s ago (a deploy's swap?)", stopped(5 * time.Second), Unknown, true},
		{"stopped 25 s ago", stopped(25 * time.Second), Down, true},
		{"job, exited 0", c("exited", "none", "no"), Unknown, false},
		{"job, running", c("running", "none", "no"), Unknown, false},
	}
	for _, tc := range cases {
		got, counted := Container(tc.c, now)
		if got != tc.want || counted != tc.counted {
			t.Errorf("%s: %v, %v; want %v, %v", tc.name, got, counted, tc.want, tc.counted)
		}
	}
}

func TestService(t *testing.T) {
	up := containers.Container{State: "running", Health: "healthy", RestartPolicy: "always"}
	down := containers.Container{State: "exited", RestartPolicy: "always"}
	job := containers.Container{State: "exited", RestartPolicy: "no"}

	if s, ok := Service([]containers.Container{up, down}, now); s != Down || !ok {
		t.Errorf("one replica down: %v, %v; want down", s, ok)
	}
	if s, ok := Service([]containers.Container{up, job}, now); s != Up || !ok {
		t.Errorf("a job beside it: %v, %v; want up", s, ok)
	}
	if _, ok := Service([]containers.Container{job}, now); ok {
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
		{"a service stopped just now", []State{Unknown, Up}, nil, Up, ContainerOnly},

		{"updating", []State{Updating, Up}, nil, Updating, ContainerOnly},
		{"updating, check failing during the swap", []State{Updating}, []State{Down}, Updating, Checked},
		{"updating, check fine", []State{Updating}, []State{Up}, Updating, Checked},
		{"updating, but another service down", []State{Updating, Down}, nil, Degraded, ContainerOnly},
		{"updating, another down, check down", []State{Updating, Down}, []State{Down}, Down, Checked},
		{"updating, another degraded, check fine", []State{Updating, Degraded}, []State{Up}, Degraded, Checked},
	}
	for _, tc := range cases {
		got, ev := Project(tc.services, tc.checks)
		if got != tc.want || ev != tc.evidence {
			t.Errorf("%s: %v, %s; want %v, %s", tc.name, got, ev, tc.want, tc.evidence)
		}
	}
}

func TestRank(t *testing.T) {
	order := []State{Unknown, Up, Updating, Degraded, Down}
	for i := 1; i < len(order); i++ {
		if !order[i].WorseThan(order[i-1]) {
			t.Errorf("%v should be worse than %v", order[i], order[i-1])
		}
	}
	if Worst(Up, Updating) != Updating || Worst(Updating, Degraded) != Degraded || Worst() != Unknown || Worst(Unknown, Up) != Up {
		t.Error("Worst ordering is wrong")
	}
	if Updating != 4 || Down != 3 {
		t.Error("stored values changed: the database keeps 0 unknown, 1 up, 2 degraded, 3 down, 4 updating")
	}
}

func TestDay(t *testing.T) {
	cases := []struct {
		up, updating, degraded, down, window int
		want                                 State
		ok                                   bool
	}{
		{1440, 0, 0, 0, 1440, Up, true},
		{1430, 0, 6, 4, 1440, Degraded, true}, // 4 down minutes aren't enough for down
		{1430, 0, 0, 10, 1440, Down, true},
		{1436, 0, 2, 2, 1440, Up, true}, // 4 bad minutes in total: a blip
		{1436, 0, 2, 3, 1440, Degraded, true},
		{1420, 20, 0, 0, 1440, Up, true}, // a long deploy doesn't colour the day
		{700, 0, 0, 0, 1440, Unknown, false},
		{700, 20, 0, 0, 1440, Up, true}, // updating minutes are observed minutes
		{300, 0, 0, 0, 600, Up, true},
		{0, 0, 0, 0, 0, Unknown, false},
	}
	for _, tc := range cases {
		got, ok := Day(tc.up, tc.updating, tc.degraded, tc.down, tc.window)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Day(%d, %d, %d, %d, %d) = %v, %v; want %v, %v", tc.up, tc.updating, tc.degraded, tc.down, tc.window, got, ok, tc.want, tc.ok)
		}
	}
}

func TestUptime(t *testing.T) {
	if pct, ok := Uptime(1438, 0, 1, 1); !ok || pct < 99.93 || pct > 99.94 {
		t.Errorf("Uptime = %v, %v; want 99.93", pct, ok)
	}
	if pct, ok := Uptime(1430, 10, 0, 0); !ok || pct != 100 {
		t.Errorf("updating minutes count as up: %v, %v", pct, ok)
	}
	if _, ok := Uptime(0, 0, 0, 0); ok {
		t.Error("nothing observed: want ok false")
	}
}

func TestStateJSON(t *testing.T) {
	data, err := json.Marshal(map[string]State{"a": Degraded, "b": Updating})
	if err != nil || string(data) != `{"a":"degraded","b":"updating"}` {
		t.Fatalf("Marshal = %s, %v", data, err)
	}
	var back map[string]State
	if err := json.Unmarshal(data, &back); err != nil || back["a"] != Degraded || back["b"] != Updating {
		t.Errorf("Unmarshal = %v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"a":"sideways"}`), &back); err == nil {
		t.Error("unknown state: want an error")
	}
}
