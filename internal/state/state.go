// Package state decides what's working, from Docker's view and the outside checks: pure functions.
package state

import (
	"fmt"
	"time"

	"github.com/lsariol/sonar/internal/containers"
)

// State is how well something works; stored as a number, ranked separately so values can be added.
type State int

const (
	Unknown  State = iota // no data
	Up                    // working
	Degraded              // working, but not fully: slow, or a service down
	Down                  // not working
	Updating              // being replaced by a deploy (counts as up)
)

var names = [...]string{"unknown", "up", "degraded", "down", "updating"}

// rank orders states from best to worst; Unknown is lowest, so Worst ignores it while there's anything else.
var rank = [...]int{Unknown: 0, Up: 1, Updating: 2, Degraded: 3, Down: 4}

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

// WorseThan reports whether s is worse than other.
func (s State) WorseThan(other State) bool {
	return rank[s] > rank[other]
}

// Worst is the worst of states; Unknown only when there's nothing else.
func Worst(states ...State) State {
	w := Unknown
	for _, s := range states {
		if s.WorseThan(w) {
			w = s
		}
	}
	return w
}

// Evidence is what a project's state rests on.
type Evidence string

const (
	Checked       Evidence = "checked"   // at least one outside check has a result
	ContainerOnly Evidence = "container" // Docker's view only
)

// StopGrace is how long a just-stopped or just-created container is unknown, since a deploy swaps them.
const StopGrace = 20 * time.Second

// Container is one container's state at now; counted is false for one-off jobs.
func Container(c containers.Container, now time.Time) (s State, counted bool) {
	if c.RestartPolicy == "no" || c.RestartPolicy == "" {
		return Unknown, false
	}
	switch c.State {
	case "running":
	case "exited", "dead", "removing":
		if c.FinishedAt != nil && now.Sub(*c.FinishedAt) < StopGrace {
			return Unknown, true
		}
		return Down, true
	case "created":
		if !c.CreatedAt.IsZero() && now.Sub(c.CreatedAt) < StopGrace {
			return Unknown, true
		}
		return Down, true
	default:
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

// Service is the worst of a service's counted containers; counted is false when all are one-off jobs.
func Service(cs []containers.Container, now time.Time) (s State, counted bool) {
	for _, c := range cs {
		if cState, ok := Container(c, now); ok {
			s, counted = Worst(s, cState), true
		}
	}
	return s, counted
}

// Check is a check's state: down after 2 failures in a row, up again on the first success.
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

// Project is a project's state from its services and checks; a deploy is updating, not an outage.
func Project(services, checks []State) (State, Evidence) {
	services, checks = known(services), known(checks)
	evidence := ContainerOnly
	if len(checks) > 0 {
		evidence = Checked
	}

	updating := false
	var settled []State
	for _, s := range services {
		if s == Updating {
			updating = true
		} else {
			settled = append(settled, s)
		}
	}
	if updating && !Worst(settled...).WorseThan(Up) {
		return Updating, evidence
	}

	if len(checks) > 0 {
		switch {
		case Worst(checks...) == Down:
			return Down, Checked
		case Worst(checks...) == Degraded || Worst(settled...).WorseThan(Up):
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
	case Worst(settled...).WorseThan(Up):
		return Degraded, ContainerOnly
	}
	return Up, ContainerOnly
}

// Day is the worst state that lasted 5 minutes; no data when under half of window was observed.
func Day(up, updating, degraded, down, window int) (s State, ok bool) {
	if window <= 0 || (up+updating+degraded+down)*2 < window {
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

// Uptime is the share of observed minutes that answered (up, updating or degraded).
func Uptime(up, updating, degraded, down int) (pct float64, ok bool) {
	observed := up + updating + degraded + down
	if observed == 0 {
		return 0, false
	}
	return float64(up+updating+degraded) / float64(observed) * 100, true
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
