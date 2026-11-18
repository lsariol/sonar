package agent

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
)

// Thresholds for Spark's conditions, in percent.
const (
	diskWarnPct, diskCritPct   = 85, 95 // of the disk used
	inodeWarnPct, inodeCritPct = 10, 5  // of inodes free
	memWarnPct, memCritPct     = 90, 95 // of memory used
	swapWarnPct, swapCritPct   = 50, 80 // of swap used
	loadWarnPerCore            = 2.0    // 15-minute load per core
)

// Lighthouse's tiers whose projects Spark itself depends on.
const infraTier, dataTier = "infra", "data"

// conditions judges Spark from the snapshot s: each thing wrong with it,
// without since (the agent adds it).
func conditions(s snapshot.Snapshot) []snapshot.Condition {
	out := []snapshot.Condition{}
	add := func(id, subject, level, msg string) {
		out = append(out, snapshot.Condition{ID: id, Subject: subject, Level: level, Message: msg})
	}

	for name, c := range s.Collectors {
		level := snapshot.Warning
		if name == dockerCollector {
			level = snapshot.Critical
		}
		switch {
		case c.State == snapshot.Unavailable && c.Error == noReadingYet:
			// starting up: not a problem yet
		case c.State == snapshot.Unavailable:
			add("collector", name, level, fmt.Sprintf("Sonar can't read %s: %s", name, c.Error))
		case c.State == snapshot.Stale:
			add("collector", name, level, fmt.Sprintf("Sonar has no reading of %s since %s", name, c.LastOK.UTC().Format(time.RFC3339)))
		}
	}

	if h := s.Host; h != nil {
		if d := h.Disk; d != nil && d.Total > 0 {
			if level := above(d.UsedPercent, diskWarnPct, diskCritPct); level != "" {
				add("disk", "", level, fmt.Sprintf("The disk is %.0f%% full: %s free of %s", d.UsedPercent, gb(d.Available), gb(d.Total)))
			}
			if d.Inodes > 0 {
				free := float64(d.InodesFree) / float64(d.Inodes) * 100
				if level := below(free, inodeWarnPct, inodeCritPct); level != "" {
					add("inodes", "", level, fmt.Sprintf("Only %.1f%% of the disk's inodes are free (%d of %d)", free, d.InodesFree, d.Inodes))
				}
			}
		}
		if h.MemoryTotal > 0 {
			if level := above(h.MemoryUsedPercent, memWarnPct, memCritPct); level != "" {
				add("memory", "", level, fmt.Sprintf("Memory is %.0f%% used: %s of %s", h.MemoryUsedPercent, gb(h.MemoryUsed), gb(h.MemoryTotal)))
			}
		}
		if h.SwapTotal > 0 {
			pct := float64(h.SwapUsed) / float64(h.SwapTotal) * 100
			if level := above(pct, swapWarnPct, swapCritPct); level != "" {
				add("swap", "", level, fmt.Sprintf("Swap is %.0f%% used: %s of %s", pct, gb(h.SwapUsed), gb(h.SwapTotal)))
			}
		}
		if cores := len(h.CorePercents); cores > 0 && h.Load15 >= loadWarnPerCore*float64(cores) {
			add("load", "", snapshot.Warning, fmt.Sprintf("The 15-minute load is %.1f on %d cores", h.Load15, cores))
		}
	}

	for _, p := range s.Projects {
		if p.Tier != infraTier && p.Tier != dataTier {
			continue
		}
		switch p.State {
		case state.Down:
			add("project", p.Project, snapshot.Critical, fmt.Sprintf("%s (%s) is down", p.Project, p.Tier))
		case state.Degraded:
			add("project", p.Project, snapshot.Warning, fmt.Sprintf("%s (%s) is degraded", p.Project, p.Tier))
		}
	}

	slices.SortFunc(out, func(x, y snapshot.Condition) int {
		return cmp.Or(cmp.Compare(rank(y.Level), rank(x.Level)), cmp.Compare(x.ID, y.ID), cmp.Compare(x.Subject, y.Subject))
	})
	return out
}

// trackConditions gives each condition the time it began: kept from the
// last snapshot while it lasts (whatever its level), now when it's new.
// Callers hold a.mu.
func (a *Agent) trackConditions(now time.Time, found []snapshot.Condition) []snapshot.Condition {
	since := make(map[string]time.Time, len(found))
	for i := range found {
		k := conditionKey(found[i])
		t, ok := a.conditionSince[k]
		if !ok {
			t = now
		}
		found[i].Since, since[k] = t, t
	}
	a.conditionSince = since
	return found
}

// resumeConditions keeps the previous agent's conditions' start times, so a
// restart doesn't make an old problem look new. Callers hold a.mu.
func (a *Agent) resumeConditions(snap snapshot.Snapshot) {
	for _, c := range snap.Conditions {
		a.conditionSince[conditionKey(c)] = c.Since
	}
}

func conditionKey(c snapshot.Condition) string {
	return c.ID + "/" + c.Subject
}

// above is the level of a value where higher is worse, "" when it's fine.
func above(v, warn, crit float64) string {
	switch {
	case v >= crit:
		return snapshot.Critical
	case v >= warn:
		return snapshot.Warning
	}
	return ""
}

// below is the level of a value where lower is worse, "" when it's fine.
func below(v, warn, crit float64) string {
	switch {
	case v < crit:
		return snapshot.Critical
	case v < warn:
		return snapshot.Warning
	}
	return ""
}

func rank(level string) int {
	switch level {
	case snapshot.Critical:
		return 2
	case snapshot.Warning:
		return 1
	}
	return 0
}

// gb is a byte count in gigabytes, as the messages show it.
func gb(n uint64) string {
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
}
