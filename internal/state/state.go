// Package state decides what's working: a container and a service from
// Docker's view, an outside check from its recent results, and a project from
// both. Pure functions, no I/O.
package state

import (
	"fmt"
	"time"

	"github.com/lsariol/sonar/internal/containers"
)

// State is how well something works. The values are what the database
// stores; how bad each is goes by rank, so Updating could be added without
// renumbering.
type State int

const (
	Unknown  State = iota // no data
	Up                    // working
	Degraded              // working, but not fully: slow, or a service down
	Down                  // not working
	Updating              // being replaced by a deploy (counts as up)
)

var names = [...]string{"unknown", "up", "degraded", "down", "updating"}

// rank orders states from best to worst; Unknown is lowest, so Worst ignores
// it while there's anything else.
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

// StopGrace is how long a container that just stopped, or was just created
// and not started yet, counts as unknown rather than down: a deploy creates
// the new container and stops the old one a moment before starting the new
// one, and a reading can land in between.
const StopGrace = 20 * time.Second

// Container is one container's state as Docker sees it at now. counted is
// false for one-off jobs (restart policy "no"), which don't count toward
// availability.
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

// Service is a compose service's state at now: the worst of its counted
// containers. counted is false when every container is a one-off job.
func Service(cs []containers.Container, now time.Time) (s State, counted bool) {
	for _, c := range cs {
		if cState, ok := Container(c, now); ok {
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
// checks. Unknown entries (no result yet) are left out. While a service is
// updating and nothing else is wrong, the project is updating, even if its
// outside check fails meanwhile: visitors may see the swap, but it's a
// deploy, not an outage.
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

// Day is a day's state from its minute counts: the worst state that lasted at
// least 5 minutes in total. Updating minutes count as up. ok is false (no
// data) when fewer than half of the window's minutes were observed; window is
// 1440, or the minutes so far today.
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

// Uptime is the percentage of observed minutes that were up, updating or
// degraded (they all answered). ok is false when nothing was observed.
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
