package api

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
	"github.com/lsariol/sonar/internal/store"
)

// excluded projects never appear in public answers; every other project
// with a compose project name does.
var excluded = []string{"sparkdb", "cloudflared"}

// IsPublic reports whether a project may appear in status:read answers.
func IsPublic(project string) bool {
	return project != "" && !slices.Contains(excluded, project)
}

// The public answers. Each is filled field by field from Sonar's data; nothing
// is copied across whole, so a field added inside Sonar never leaks out.

type statusAnswer struct {
	GeneratedAt *time.Time      `json:"generated_at"`
	Fresh       bool            `json:"fresh"`
	Overall     overallStatus   `json:"overall"`
	Projects    []projectStatus `json:"projects"`
}

type overallStatus struct {
	State     string `json:"state"`
	Uptime30d Uptime `json:"uptime_30d"`
}

type projectStatus struct {
	Project    string     `json:"project"`
	State      string     `json:"state"`
	Evidence   string     `json:"evidence"`
	ResponseMs *int       `json:"response_ms"`
	Uptime30d  Uptime     `json:"uptime_30d"`
	Uptime90d  Uptime     `json:"uptime_90d"`
	Days       []DayState `json:"days"`
}

// Uptime is the share of observed time a project answered, and how much of
// the window was observed.
type Uptime struct {
	Pct         *float64 `json:"pct"` // nil under half coverage
	CoveragePct float64  `json:"coverage_pct"`
}

// DayState is one UTC day of a project's history.
type DayState struct {
	Date        string   `json:"date"`
	State       string   `json:"state"`
	UptimePct   *float64 `json:"uptime_pct"`
	CoveragePct float64  `json:"coverage_pct"`
}

type historyAnswer struct {
	Range         string      `json:"range"`
	From          time.Time   `json:"from"`
	To            time.Time   `json:"to"`
	BucketSeconds int         `json:"bucket_seconds"`
	GeneratedAt   time.Time   `json:"generated_at"`
	Points        []hostPoint `json:"points"`
}

type hostPoint struct {
	T           time.Time `json:"t"`
	CPUPct      *spread   `json:"cpu_pct"`
	MemUsedPct  *spread   `json:"mem_used_pct"`
	CoveragePct float64   `json:"coverage_pct"`
}

type spread struct {
	Avg float64 `json:"avg"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// publicState is a state as the status page names it.
func publicState(s state.State) string {
	switch s {
	case state.Up:
		return "operational"
	case state.Degraded:
		return "degraded"
	case state.Down:
		return "down"
	}
	return "unknown"
}

// window is a stretch of whole UTC days ending today, with today counted up
// to now.
type window struct {
	first   time.Time // the first day, at midnight UTC
	days    int
	minutes int // minutes in the window so far
}

func windowOf(now time.Time, days int) window {
	today := midnight(now)
	sinceMidnight := max(1, int(now.Sub(today)/time.Minute))
	return window{first: today.AddDate(0, 0, -(days - 1)), days: days, minutes: (days-1)*1440 + sinceMidnight}
}

func midnight(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// uptimeOver is uptime and coverage from summed minute counts over a window
// of the given number of minutes.
func uptimeOver(up, degraded, down, minutes int) Uptime {
	u := Uptime{CoveragePct: round(pct(up+degraded+down, minutes), 1)}
	if (up+degraded+down)*2 >= minutes {
		if p, ok := state.Uptime(up, degraded, down); ok {
			p = round(p, 2)
			u.Pct = &p
		}
	}
	return u
}

// ProjectDays builds a project's strip of days, oldest first, from its daily
// rows, and its uptime over the last 30 and 90 days. The CLI shows the same
// figures, so the status page and `sonar days` always agree.
func ProjectDays(now time.Time, days int, rows []store.DayCounts) (strip []DayState, up30, up90 Uptime) {
	byDay := make(map[time.Time]store.DayCounts, len(rows))
	for _, r := range rows {
		byDay[midnight(r.Day)] = r
	}
	today := midnight(now)
	sinceMidnight := max(1, int(now.Sub(today)/time.Minute))

	strip = make([]DayState, 0, days)
	w := windowOf(now, days)
	for d := 0; d < days; d++ {
		date := w.first.AddDate(0, 0, d)
		minutes := 1440
		if date.Equal(today) {
			minutes = sinceMidnight
		}
		r := byDay[date]
		observed := r.Up + r.Degraded + r.Down
		ds := DayState{Date: date.Format(time.DateOnly), State: "no_data", CoveragePct: round(pct(observed, minutes), 1)}
		if s, ok := state.Day(r.Up, r.Degraded, r.Down, minutes); ok {
			ds.State = publicState(s)
			if p, ok := state.Uptime(r.Up, r.Degraded, r.Down); ok {
				p = round(p, 2)
				ds.UptimePct = &p
			}
		}
		strip = append(strip, ds)
	}

	sum := func(window window) Uptime {
		var up, degraded, down int
		for date, r := range byDay {
			if !date.Before(window.first) {
				up, degraded, down = up+r.Up, degraded+r.Degraded, down+r.Down
			}
		}
		return uptimeOver(up, degraded, down, window.minutes)
	}
	return strip, sum(windowOf(now, 30)), sum(windowOf(now, 90))
}

// buildStatus turns the snapshot and the daily rows into the public status.
func buildStatus(now time.Time, snap *snapshot.Snapshot, days int, rows []store.DayCounts, response map[string]int) statusAnswer {
	answer := statusAnswer{Overall: overallStatus{State: "unknown"}, Projects: []projectStatus{}}
	fresh := snap != nil && snap.Fresh(now)
	answer.Fresh = fresh
	if snap != nil {
		at := snap.GeneratedAt.UTC()
		answer.GeneratedAt = &at
	}

	byProject := make(map[string][]store.DayCounts)
	for _, r := range rows {
		byProject[r.Project] = append(byProject[r.Project], r)
	}

	worst := state.Unknown
	var up, degraded, down, count int
	if snap != nil {
		for _, p := range snap.Projects {
			if !IsPublic(p.Project) || !slices.ContainsFunc(p.Services, func(s snapshot.Service) bool { return s.Counted }) {
				continue // excluded, or only one-off jobs: nothing to be up or down
			}
			st := p.State
			if !fresh {
				st = state.Unknown
			}
			strip, up30, up90 := ProjectDays(now, days, byProject[p.Project])
			ps := projectStatus{Project: p.Project, State: publicState(st), Evidence: string(p.Evidence),
				Uptime30d: up30, Uptime90d: up90, Days: strip}
			if ms, ok := response[p.Project]; ok {
				ps.ResponseMs = &ms
			}
			answer.Projects = append(answer.Projects, ps)
			worst = state.Worst(worst, st)

			first := windowOf(now, 30).first
			for _, r := range byProject[p.Project] {
				if !midnight(r.Day).Before(first) {
					up, degraded, down = up+r.Up, degraded+r.Degraded, down+r.Down
				}
			}
			count++
		}
	}
	if fresh && count > 0 {
		answer.Overall.State = publicState(worst)
	}
	answer.Overall.Uptime30d = uptimeOver(up, degraded, down, max(1, count)*windowOf(now, 30).minutes)
	slices.SortFunc(answer.Projects, func(a, b projectStatus) int { return cmp.Compare(a.Project, b.Project) })
	return answer
}

// historyRange is one choice of graph range.
type historyRange struct {
	length, bucket time.Duration
}

var historyRanges = map[string]historyRange{
	"1h":  {time.Hour, time.Minute},
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute},
	"30d": {30 * 24 * time.Hour, 2 * time.Hour},
}

// historySpan is the range's window: it ends with the bucket holding now, so
// the newest, still-filling bucket is the last point.
func historySpan(now time.Time, r historyRange) (from, to time.Time) {
	to = now.UTC().Truncate(r.bucket).Add(r.bucket)
	return to.Add(-r.length), to
}

// buildHistory lays the buckets out as points, one per bucket in the range;
// a bucket without data is a gap (null values).
func buildHistory(now time.Time, name string, r historyRange, buckets []store.HostBucket) historyAnswer {
	from, to := historySpan(now, r)
	answer := historyAnswer{Range: name, From: from, To: to, BucketSeconds: int(r.bucket / time.Second),
		GeneratedAt: now.UTC(), Points: make([]hostPoint, 0, int(r.length/r.bucket))}

	byStart := make(map[time.Time]store.HostBucket, len(buckets))
	for _, b := range buckets {
		byStart[b.Start.UTC()] = b
	}
	full := int(r.bucket / time.Minute * 12)
	for t := from; t.Before(to); t = t.Add(r.bucket) {
		p := hostPoint{T: t}
		if b, ok := byStart[t]; ok {
			p.CoveragePct = round(math.Min(100, pct(b.Samples, full)), 1)
			if b.CPUAvg != nil && b.CPUMin != nil && b.CPUMax != nil {
				p.CPUPct = &spread{Avg: round(*b.CPUAvg, 2), Min: round(*b.CPUMin, 2), Max: round(*b.CPUMax, 2)}
			}
			if b.MemPctAvg != nil && b.MemPctMin != nil && b.MemMax != nil {
				p.MemUsedPct = &spread{Avg: round(*b.MemPctAvg, 2), Min: round(*b.MemPctMin, 2), Max: round(*b.MemMax, 2)}
			}
		}
		answer.Points = append(answer.Points, p)
	}
	return answer
}

func pct(part, whole int) float64 {
	if whole <= 0 {
		return 0
	}
	return math.Min(100, float64(part)/float64(whole)*100)
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
