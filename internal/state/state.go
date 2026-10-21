// Package state decides what's working: a container and a service from
// Docker's view, an outside check from its recent results, and a project from
// both. Pure functions, no I/O.
package state

import (
	"fmt"

	"github.com/lsariol/sonar/internal/containers"
)

// State is how well something works. A higher value is worse; Unknown (no
// data) is the lowest, so Worst ignores it while there's anything else.
type State int

const (
	Unknown State = iota
	Up
	Degraded
	Down
)

var names = [...]string{"unknown", "up", "degraded", "down"}

func (s State) String() string {
	if s < 0 || int(s) >= len(names) {
		return fmt.Sprintf("State(%d)", int(s))
	}
	return names[s]
}

func (s State) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

func (s *State) UnmarshalText(b []byte) error {
	for i, n := range names {
		if n == string(b) {
			*s = State(i)
			return nil
		}
	}
	return fmt.Errorf("state: unknown state %q", b)
}

// Worst is the worst of states; Unknown only when there's nothing else.
func Worst(states ...State) State {
	w := Unknown
	for _, s := range states {
		w = max(w, s)
	}
	return w
}

// Evidence is what a project's state rests on.
type Evidence string

const (
	Checked       Evidence = "checked"   // at least one outside check has a result
	ContainerOnly Evidence = "container" // Docker's view only
)

// Container is one container's state as Docker sees it. counted is false for
// one-off jobs (restart policy "no"), which don't count toward availability.
func Container(c containers.Container) (s State, counted bool) {
	if c.RestartPolicy == "no" || c.RestartPolicy == "" {
		return Unknown, false
	}
	if c.State != "running" {
		return Down, true
	}
	switch c.Health {
	case "unhealthy":
		return Down, true
	case "starting":
		return Degraded, true
	}
	return Up, true
}

// Service is a compose service's state: the worst of its counted containers.
// counted is false when every container is a one-off job.
func Service(cs []containers.Container) (s State, counted bool) {
	for _, c := range cs {
		if cState, ok := Container(c); ok {
			s, counted = Worst(s, cState), true
		}
	}
	return s, counted
}

// Check is an outside check's state after its latest result: down only after
// 2 failures in a row, so one blip changes nothing; up again on the first
// success.
func Check(prev State, failuresInARow int, slow bool) State {
	switch {
	case failuresInARow >= 2:
		return Down
	case failuresInARow == 1:
		return prev
	case slow:
		return Degraded
	}
	return Up
}

// Project is a project's state from its counted services and its outside
// checks. Unknown entries (no result yet) are left out.
func Project(services, checks []State) (State, Evidence) {
	services, checks = known(services), known(checks)

	if len(checks) > 0 {
		switch {
		case Worst(checks...) == Down:
			return Down, Checked
		case Worst(checks...) == Degraded || Worst(services...) > Up:
			return Degraded, Checked
		}
		return Up, Checked
	}

	if len(services) == 0 {
		return Unknown, ContainerOnly
	}
	down := 0
	for _, s := range services {
		if s == Down {
			down++
		}
	}
	switch {
	case down == len(services):
		return Down, ContainerOnly
	case Worst(services...) > Up:
		return Degraded, ContainerOnly
	}
	return Up, ContainerOnly
}

// Day is a day's state from its minute counts: the worst state that lasted at
// least 5 minutes in total. ok is false (no data) when fewer than half of the
// window's minutes were observed; window is 1440, or the minutes so far today.
func Day(up, degraded, down, window int) (s State, ok bool) {
	if window <= 0 || (up+degraded+down)*2 < window {
		return Unknown, false
	}
	switch {
	case down >= 5:
		return Down, true
	case down+degraded >= 5:
		return Degraded, true
	}
	return Up, true
}

// Uptime is the percentage of observed minutes that were up or degraded
// (degraded still answered). ok is false when nothing was observed.
func Uptime(up, degraded, down int) (pct float64, ok bool) {
	observed := up + degraded + down
	if observed == 0 {
		return 0, false
	}
	return float64(up+degraded) / float64(observed) * 100, true
}

func known(states []State) []State {
	var out []State
	for _, s := range states {
		if s != Unknown {
			out = append(out, s)
		}
	}
	return out
}
