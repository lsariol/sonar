package api

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
	"github.com/lsariol/sonar/internal/store"
)

// The monitor:read answers: owner detail for Vista. Like the public ones,
// each is filled field by field; nothing internal is served as is.

// --- GET /v1/monitor/projects ---

type monitorProjectsAnswer struct {
	GeneratedAt *time.Time       `json:"generated_at"`
	Fresh       bool             `json:"fresh"`
	Projects    []monitorProject `json:"projects"`
}

type monitorProject struct {
	Project         string           `json:"project"`
	Tier            *string          `json:"tier"` // nil until Lighthouse labels the containers
	State           string           `json:"state"`
	StateSince      *time.Time       `json:"state_since"`       // nil when unknown
	StateSinceExact bool             `json:"state_since_exact"` // false: at least since then (the history starts there)
	Evidence        string           `json:"evidence"`
	Version         *string          `json:"version"`
	Services        []monitorService `json:"services"`
	Checks          []monitorCheck   `json:"checks"`
}

type monitorService struct {
	Service    string             `json:"service"`
	State      string             `json:"state"`
	Counted    bool               `json:"counted"` // false for one-off jobs
	Update     *monitorUpdate     `json:"update"`  // a deploy in progress
	Usage      monitorUsage       `json:"usage"`
	Containers []monitorContainer `json:"containers"`
}

type monitorUpdate struct {
	Since time.Time `json:"since"`
	From  *string   `json:"from"`
	To    *string   `json:"to"`
}

type monitorUsage struct {
	CPUCores     *float64 `json:"cpu_cores"`
	MemoryBytes  *uint64  `json:"memory_bytes"`
	NetRxRate    *float64 `json:"net_rx_bytes_per_s"`
	NetTxRate    *float64 `json:"net_tx_bytes_per_s"`
	BlkReadRate  *float64 `json:"blk_read_bytes_per_s"`
	BlkWriteRate *float64 `json:"blk_write_bytes_per_s"`
}

type monitorContainer struct {
	Name         string       `json:"name"`
	Image        string       `json:"image"`
	Version      *string      `json:"version"`
	State        string       `json:"state"`  // Docker's: running, exited, ...
	Health       string       `json:"health"` // healthy, unhealthy, starting, none
	ExitCode     int          `json:"exit_code"`
	RestartCount int          `json:"restart_count"`
	CreatedAt    time.Time    `json:"created_at"`
	StartedAt    *time.Time   `json:"started_at"`
	FinishedAt   *time.Time   `json:"finished_at"`
	CPULimit     *float64     `json:"cpu_limit"`    // cores; nil when unlimited
	MemoryLimit  *int64       `json:"memory_limit"` // bytes; nil when unlimited
	Usage        monitorUsage `json:"usage"`
}

type monitorCheck struct {
	Service             string     `json:"service"`
	URL                 string     `json:"url"`
	State               string     `json:"state"`
	LastAt              *time.Time `json:"last_at"`
	LastOutcome         *string    `json:"last_outcome"` // ok, slow or failed
	LastStatus          *int       `json:"last_status"`  // HTTP status; nil without an answer
	LastLatencyMs       *int64     `json:"last_latency_ms"`
	LastError           *string    `json:"last_error"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
}

// buildMonitorProjects is every project as it is now; since comes from the
// history.
func buildMonitorProjects(now time.Time, snap *snapshot.Snapshot, runs map[string]store.StateRun) monitorProjectsAnswer {
	answer := monitorProjectsAnswer{Projects: []monitorProject{}}
	if snap == nil {
		return answer
	}
	at := snap.GeneratedAt.UTC()
	answer.GeneratedAt, answer.Fresh = &at, snap.Fresh(now)

	checksOf := make(map[string][]monitorCheck)
	for _, c := range snap.Checks {
		mc := monitorCheck{Service: c.Service, URL: c.URL, State: publicState(c.State), LastAt: c.LastAt,
			LastLatencyMs: c.LastLatencyMs, LastError: optString(c.LastError), LastOutcome: optString(c.LastOutcome),
			ConsecutiveFailures: c.ConsecutiveFailures}
		if c.LastStatus != 0 {
			st := c.LastStatus
			mc.LastStatus = &st
		}
		checksOf[c.Project] = append(checksOf[c.Project], mc)
	}

	for _, p := range snap.Projects {
		if !anyProject(p.Project) {
			continue
		}
		st := p.State
		if !answer.Fresh {
			st = state.Unknown
		}
		mp := monitorProject{Project: p.Project, Tier: optString(p.Tier), State: publicState(st), Evidence: string(p.Evidence),
			Version: projectVersion(p), Services: []monitorService{}, Checks: checksOf[p.Project]}
		if mp.Checks == nil {
			mp.Checks = []monitorCheck{}
		}
		if run, ok := runs[p.Project]; ok && st != state.Unknown {
			switch {
			case run.LastOther != nil:
				since := run.LastOther.Add(time.Minute)
				mp.StateSince, mp.StateSinceExact = &since, true
			case run.First != nil:
				mp.StateSince = run.First
			}
		}
		for _, s := range p.Services {
			ms := monitorService{Service: s.Service, State: publicState(s.State), Counted: s.Counted,
				Usage: toUsage(s.Usage), Containers: []monitorContainer{}}
			if !answer.Fresh {
				ms.State = publicState(state.Unknown)
			}
			if u := s.Update; u != nil {
				ms.Update = &monitorUpdate{Since: u.Since.UTC(), From: optString(u.From), To: optString(u.To)}
			}
			for _, c := range s.Containers {
				mc := monitorContainer{Name: c.Name, Image: c.Image, Version: optString(c.Version), State: c.State,
					Health: c.Health, ExitCode: c.ExitCode, RestartCount: c.RestartCount, CreatedAt: c.CreatedAt.UTC(),
					StartedAt: c.StartedAt, FinishedAt: c.FinishedAt, Usage: toUsage(c.Usage)}
				if c.CPULimit > 0 {
					l := c.CPULimit
					mc.CPULimit = &l
				}
				if c.MemoryLimit > 0 {
					l := c.MemoryLimit
					mc.MemoryLimit = &l
				}
				ms.Containers = append(ms.Containers, mc)
			}
			mp.Services = append(mp.Services, ms)
		}
		answer.Projects = append(answer.Projects, mp)
	}
	return answer
}

func toUsage(u snapshot.Usage) monitorUsage {
	return monitorUsage{CPUCores: u.CPUCores, MemoryBytes: u.MemoryBytes, NetRxRate: u.NetRxRate, NetTxRate: u.NetTxRate,
		BlkReadRate: u.BlkReadRate, BlkWriteRate: u.BlkWriteRate}
}

// currentStates is each project's state now, for looking up since when.
func currentStates(now time.Time, snap *snapshot.Snapshot) map[string]state.State {
	out := make(map[string]state.State)
	if snap == nil || !snap.Fresh(now) {
		return out
	}
	for _, p := range snap.Projects {
		if anyProject(p.Project) && p.State != state.Unknown {
			out[p.Project] = p.State
		}
	}
	return out
}

// --- GET /v1/monitor/conditions ---

type conditionsAnswer struct {
	GeneratedAt *time.Time        `json:"generated_at"`
	Fresh       bool              `json:"fresh"`
	State       string            `json:"state"` // ok, warning, critical, or unknown without a snapshot
	Conditions  []conditionAnswer `json:"conditions"`
	Collectors  []collectorAnswer `json:"collectors"`
	Attention   []string          `json:"attention"` // configuration problems, such as a refused label
}

type conditionAnswer struct {
	ID      string    `json:"id"`
	Subject *string   `json:"subject"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

type collectorAnswer struct {
	Name   string     `json:"name"`
	State  string     `json:"state"` // available, unavailable, stale, unsupported
	LastOK *time.Time `json:"last_ok"`
	Error  *string    `json:"error"`
}

// buildConditions is Spark's own state: the agent's conditions, plus the
// agent itself when its snapshot is stale.
func buildConditions(now time.Time, snap *snapshot.Snapshot) conditionsAnswer {
	answer := conditionsAnswer{State: "unknown", Conditions: []conditionAnswer{}, Collectors: []collectorAnswer{}, Attention: []string{}}
	if snap == nil {
		return answer
	}
	at := snap.GeneratedAt.UTC()
	answer.GeneratedAt, answer.Fresh = &at, snap.Fresh(now)

	if !answer.Fresh {
		answer.Conditions = append(answer.Conditions, conditionAnswer{ID: "agent", Level: snapshot.Critical, Since: at,
			Message: fmt.Sprintf("Sonar's agent hasn't written a snapshot since %s: everything else here is out of date", at.Format(time.RFC3339))})
	}
	for _, c := range snap.Conditions {
		answer.Conditions = append(answer.Conditions, conditionAnswer{ID: c.ID, Subject: optString(c.Subject), Level: c.Level,
			Message: c.Message, Since: c.Since.UTC()})
	}
	answer.State = "ok"
	for _, c := range answer.Conditions {
		if c.Level == snapshot.Critical || answer.State == "ok" {
			answer.State = c.Level
		}
	}

	names := make([]string, 0, len(snap.Collectors))
	for name := range snap.Collectors {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		c := snap.Collectors[name]
		answer.Collectors = append(answer.Collectors, collectorAnswer{Name: name, State: c.State, LastOK: c.LastOK, Error: optString(c.Error)})
	}
	answer.Attention = append(answer.Attention, snap.Attention...)
	return answer
}

// --- GET /v1/monitor/host ---

type monitorHostAnswer struct {
	Range         string            `json:"range"`
	From          time.Time         `json:"from"`
	To            time.Time         `json:"to"`
	BucketSeconds int               `json:"bucket_seconds"`
	GeneratedAt   time.Time         `json:"generated_at"`
	Now           *hostNow          `json:"now"` // nil until the agent has read the host
	Points        []hostDetailPoint `json:"points"`
	Network       []netSeries       `json:"network"` // per interface; empty without the host's network counters
	Updates       []publicUpdate    `json:"updates"` // every project's
}

type netSeries struct {
	Interface string     `json:"interface"`
	Points    []netPoint `json:"points"` // only buckets with data
}

type netPoint struct {
	T       time.Time `json:"t"`
	Minutes int       `json:"minutes"`
	RxBytes int64     `json:"rx_bytes"` // moved during the bucket
	TxBytes int64     `json:"tx_bytes"`
}

type netNow struct {
	Interface   string   `json:"interface"`
	RxBytesPerS *float64 `json:"rx_bytes_per_s"`
	TxBytesPerS *float64 `json:"tx_bytes_per_s"`
}

type hostNow struct {
	ReadAt        time.Time  `json:"read_at"`
	CPUPct        *float64   `json:"cpu_pct"`
	CorePcts      []float64  `json:"core_pcts"`
	MemoryTotal   uint64     `json:"memory_total"`
	MemoryUsed    uint64     `json:"memory_used"`
	MemoryUsedPct float64    `json:"memory_used_pct"`
	SwapTotal     uint64     `json:"swap_total"`
	SwapUsed      uint64     `json:"swap_used"`
	Load1         float64    `json:"load1"`
	Load5         float64    `json:"load5"`
	Load15        float64    `json:"load15"`
	UptimeSeconds float64    `json:"uptime_seconds"`
	BootedAt      *time.Time `json:"booted_at"`
	Disk          *diskNow   `json:"disk"`
	Network       []netNow   `json:"network"`
}

type diskNow struct {
	Total      uint64  `json:"total"`
	Used       uint64  `json:"used"`
	Available  uint64  `json:"available"`
	UsedPct    float64 `json:"used_pct"`
	Inodes     uint64  `json:"inodes"`
	InodesFree uint64  `json:"inodes_free"`
}

type hostDetailPoint struct {
	T              time.Time `json:"t"`
	CoveragePct    float64   `json:"coverage_pct"`
	CPUPct         *spread   `json:"cpu_pct"`
	MemUsedPct     *spread   `json:"mem_used_pct"`
	MemUsedBytes   *int64    `json:"mem_used_bytes"`
	MemTotalBytes  *int64    `json:"mem_total_bytes"`
	SwapUsedBytes  *int64    `json:"swap_used_bytes"`
	Load1          *peak     `json:"load1"`
	DiskUsedBytes  *int64    `json:"disk_used_bytes"`
	DiskTotalBytes *int64    `json:"disk_total_bytes"`
}

// peak is an average and the highest value.
type peak struct {
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

func toHostNow(h *snapshot.Host) *hostNow {
	if h == nil {
		return nil
	}
	n := &hostNow{ReadAt: h.ReadAt.UTC(), CPUPct: roundPtr(h.CPUPercent, 2), CorePcts: []float64{},
		MemoryTotal: h.MemoryTotal, MemoryUsed: h.MemoryUsed, MemoryUsedPct: round(h.MemoryUsedPercent, 2),
		SwapTotal: h.SwapTotal, SwapUsed: h.SwapUsed, Load1: h.Load1, Load5: h.Load5, Load15: h.Load15,
		UptimeSeconds: h.UptimeSeconds}
	for _, c := range h.CorePercents {
		n.CorePcts = append(n.CorePcts, round(c, 2))
	}
	if h.UptimeSeconds > 0 {
		booted := h.ReadAt.Add(-time.Duration(h.UptimeSeconds * float64(time.Second))).UTC().Truncate(time.Second)
		n.BootedAt = &booted
	}
	n.Network = []netNow{}
	for _, i := range h.Network {
		n.Network = append(n.Network, netNow{Interface: i.Name, RxBytesPerS: roundPtr(i.RxRate, 1), TxBytesPerS: roundPtr(i.TxRate, 1)})
	}
	if d := h.Disk; d != nil {
		n.Disk = &diskNow{Total: d.Total, Used: d.Used, Available: d.Available, UsedPct: round(d.UsedPercent, 2),
			Inodes: d.Inodes, InodesFree: d.InodesFree}
	}
	return n
}

// buildMonitorHost lays the host's buckets out as points, one per bucket in
// the range, a gap where there's no data.
func buildMonitorHost(now time.Time, name string, r historyRange, snap *snapshot.Snapshot, buckets []store.HostDetailBucket,
	network []store.NetBucket, updates []store.Update) monitorHostAnswer {
	from, to := historySpan(now, r)
	answer := monitorHostAnswer{Range: name, From: from, To: to, BucketSeconds: int(r.bucket / time.Second),
		GeneratedAt: now.UTC(), Points: make([]hostDetailPoint, 0, int(r.length/r.bucket)), Network: []netSeries{},
		Updates: []publicUpdate{}}
	for _, b := range network {
		n := len(answer.Network)
		if n == 0 || answer.Network[n-1].Interface != b.Interface {
			answer.Network = append(answer.Network, netSeries{Interface: b.Interface, Points: []netPoint{}})
			n++
		}
		answer.Network[n-1].Points = append(answer.Network[n-1].Points,
			netPoint{T: b.Start, Minutes: b.Minutes, RxBytes: b.RxBytes, TxBytes: b.TxBytes})
	}
	if snap != nil {
		answer.Now = toHostNow(snap.Host)
	}
	for _, u := range mergeUpdates(updates, anyProject) {
		if !u.Started.Before(from) && u.Started.Before(to) {
			answer.Updates = append(answer.Updates, u)
		}
	}
	byStart := make(map[time.Time]store.HostDetailBucket, len(buckets))
	for _, b := range buckets {
		byStart[b.Start.UTC()] = b
	}
	full := int(r.bucket / time.Minute * 12)
	for t := from; t.Before(to); t = t.Add(r.bucket) {
		p := hostDetailPoint{T: t}
		if b, ok := byStart[t]; ok {
			p.CoveragePct = round(min(100, pct(b.Samples, full)), 1)
			if b.CPUAvg != nil && b.CPUMin != nil && b.CPUMax != nil {
				p.CPUPct = &spread{Avg: round(*b.CPUAvg, 2), Min: round(*b.CPUMin, 2), Max: round(*b.CPUMax, 2)}
			}
			if b.MemPctAvg != nil && b.MemPctMin != nil && b.MemMax != nil {
				p.MemUsedPct = &spread{Avg: round(*b.MemPctAvg, 2), Min: round(*b.MemPctMin, 2), Max: round(*b.MemMax, 2)}
			}
			p.MemUsedBytes, p.MemTotalBytes, p.SwapUsedBytes = b.MemUsedAvg, b.MemTotal, b.SwapUsedAvg
			if b.Load1Avg != nil && b.Load1Max != nil {
				p.Load1 = &peak{Avg: round(*b.Load1Avg, 2), Max: round(*b.Load1Max, 2)}
			}
			p.DiskUsedBytes, p.DiskTotalBytes = b.DiskUsed, b.DiskTotal
		}
		answer.Points = append(answer.Points, p)
	}
	return answer
}

// --- GET /v1/monitor/usage ---

type usageAnswer struct {
	Range         string         `json:"range"`
	From          time.Time      `json:"from"`
	To            time.Time      `json:"to"`
	BucketSeconds int            `json:"bucket_seconds"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Services      []serviceUsage `json:"services"`
}

type serviceUsage struct {
	Project string       `json:"project"`
	Service string       `json:"service"`
	Points  []usagePoint `json:"points"` // only buckets with data
}

type usagePoint struct {
	T             time.Time `json:"t"`
	Minutes       int       `json:"minutes"` // minutes with a reading
	CPUCores      *peak     `json:"cpu_cores"`
	MemoryBytes   *peakInt  `json:"memory_bytes"`
	NetRxBytes    int64     `json:"net_rx_bytes"` // moved during the bucket
	NetTxBytes    int64     `json:"net_tx_bytes"`
	BlkReadBytes  int64     `json:"blk_read_bytes"`
	BlkWriteBytes int64     `json:"blk_write_bytes"`
}

type peakInt struct {
	Avg int64 `json:"avg"`
	Max int64 `json:"max"`
}

func buildUsage(now time.Time, name string, r historyRange, buckets []store.UsageBucket) usageAnswer {
	from, to := historySpan(now, r)
	answer := usageAnswer{Range: name, From: from, To: to, BucketSeconds: int(r.bucket / time.Second), GeneratedAt: now.UTC(),
		Services: []serviceUsage{}}
	for _, b := range buckets {
		if !anyProject(b.Project) {
			continue
		}
		n := len(answer.Services)
		if n == 0 || answer.Services[n-1].Project != b.Project || answer.Services[n-1].Service != b.Service {
			answer.Services = append(answer.Services, serviceUsage{Project: b.Project, Service: b.Service, Points: []usagePoint{}})
			n++
		}
		p := usagePoint{T: b.Start, Minutes: b.Minutes, NetRxBytes: b.NetRx, NetTxBytes: b.NetTx, BlkReadBytes: b.BlkRead, BlkWriteBytes: b.BlkWrite}
		if b.CPUAvg != nil && b.CPUMax != nil {
			p.CPUCores = &peak{Avg: round(*b.CPUAvg, 4), Max: round(*b.CPUMax, 4)}
		}
		if b.MemAvg != nil && b.MemMax != nil {
			p.MemoryBytes = &peakInt{Avg: *b.MemAvg, Max: *b.MemMax}
		}
		answer.Services[n-1].Points = append(answer.Services[n-1].Points, p)
	}
	return answer
}

// --- GET /v1/monitor/states ---

// mergeGap is the longest stretch without data that doesn't break a span:
// the agent's own restart, or a short pause, isn't news.
const mergeGap = 5 * time.Minute

type statesAnswer struct {
	Range       string          `json:"range"`
	From        time.Time       `json:"from"`
	To          time.Time       `json:"to"`
	GeneratedAt time.Time       `json:"generated_at"`
	Projects    []projectStates `json:"projects"`
}

type projectStates struct {
	Project string      `json:"project"`
	Spans   []stateSpan `json:"spans"`
}

// stateSpan is a stretch in one state. Unknown spans are gaps in the
// history longer than mergeGap.
type stateSpan struct {
	State           string       `json:"state"`
	Start           time.Time    `json:"start"`
	End             time.Time    `json:"end"`
	ObservedMinutes int          `json:"observed_minutes"`
	Services        []spanCause  `json:"services"` // services that weren't up during it
	Checks          []checkCause `json:"checks"`   // outside checks that failed or were slow
}

type spanCause struct {
	Service string `json:"service"`
	State   string `json:"state"` // the worst seen
	Minutes int    `json:"minutes"`
}

type checkCause struct {
	Service   string  `json:"service"`
	URL       string  `json:"url"`
	State     string  `json:"state"`
	Failures  int     `json:"failures"`
	Slow      int     `json:"slow"`
	LastError *string `json:"last_error"`
}

// buildStates turns runs of minutes into each project's spans, joined
// across short gaps, with what wasn't up during each.
func buildStates(now time.Time, name string, length time.Duration, spans []store.Span, services []store.ServiceTrouble, checks []store.CheckTrouble) statesAnswer {
	to := now.UTC().Truncate(time.Minute).Add(time.Minute)
	from := to.Add(-length)
	answer := statesAnswer{Range: name, From: from, To: to, GeneratedAt: now.UTC(), Projects: []projectStates{}}

	for _, sp := range spans {
		if !anyProject(sp.Project) {
			continue
		}
		n := len(answer.Projects)
		if n == 0 || answer.Projects[n-1].Project != sp.Project {
			answer.Projects = append(answer.Projects, projectStates{Project: sp.Project, Spans: []stateSpan{}})
			n++
		}
		ps := &answer.Projects[n-1]
		st := publicState(sp.State)
		if k := len(ps.Spans); k > 0 {
			last := &ps.Spans[k-1]
			gap := sp.Start.Sub(last.End)
			if last.State == st && gap <= mergeGap {
				last.End, last.ObservedMinutes = sp.End, last.ObservedMinutes+sp.Minutes
				continue
			}
			if gap > mergeGap {
				ps.Spans = append(ps.Spans, stateSpan{State: "unknown", Start: last.End, End: sp.Start})
			}
		}
		ps.Spans = append(ps.Spans, stateSpan{State: st, Start: sp.Start, End: sp.End, ObservedMinutes: sp.Minutes})
	}

	for i := range answer.Projects {
		ps := &answer.Projects[i]
		for j := range ps.Spans {
			sp := &ps.Spans[j]
			sp.Services, sp.Checks = []spanCause{}, []checkCause{}
			if sp.State == "operational" || sp.State == "unknown" {
				continue
			}
			worst := make(map[string]state.State)
			minutes := make(map[string]int)
			for _, t := range services {
				if t.Project == ps.Project && !t.Minute.Before(sp.Start) && t.Minute.Before(sp.End) {
					worst[t.Service] = state.Worst(worst[t.Service], t.State)
					minutes[t.Service]++
				}
			}
			for svc, st := range worst {
				sp.Services = append(sp.Services, spanCause{Service: svc, State: publicState(st), Minutes: minutes[svc]})
			}
			slices.SortFunc(sp.Services, func(x, y spanCause) int { return cmp.Compare(x.Service, y.Service) })

			byCheck := make(map[[2]string]*checkCause)
			worstCheck := make(map[[2]string]state.State)
			var order [][2]string
			for _, t := range checks {
				if t.Project != ps.Project || t.Minute.Before(sp.Start) || !t.Minute.Before(sp.End) {
					continue
				}
				k := [2]string{t.Service, t.URL}
				c := byCheck[k]
				if c == nil {
					c = &checkCause{Service: t.Service, URL: t.URL, State: publicState(state.Unknown)}
					byCheck[k] = c
					order = append(order, k)
				}
				c.Failures += t.Failures
				c.Slow += t.Slow
				worstCheck[k] = state.Worst(worstCheck[k], t.State)
				c.State = publicState(worstCheck[k])
				if t.LastError != "" {
					e := t.LastError
					c.LastError = &e
				}
			}
			slices.SortFunc(order, func(x, y [2]string) int { return cmp.Or(cmp.Compare(x[0], y[0]), cmp.Compare(x[1], y[1])) })
			for _, k := range order {
				sp.Checks = append(sp.Checks, *byCheck[k])
			}
		}
	}
	return answer
}

// statesRanges are the ranges /v1/monitor/states answers: the minutes are
// kept for 30 days.
var statesRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func roundPtr(v *float64, places int) *float64 {
	if v == nil {
		return nil
	}
	r := round(*v, places)
	return &r
}

// --- GET /v1/monitor/databases ---

type databasesAnswer struct {
	Range         string           `json:"range"`
	From          time.Time        `json:"from"`
	To            time.Time        `json:"to"`
	BucketSeconds int              `json:"bucket_seconds"`
	GeneratedAt   time.Time        `json:"generated_at"`
	Now           *databaseServer  `json:"now"` // nil until sparkdb has been read
	Databases     []databaseSeries `json:"databases"`
}

type databaseServer struct {
	ReadAt         time.Time     `json:"read_at"`
	MaxConnections int           `json:"max_connections"`
	Connections    int           `json:"connections"`
	Databases      []databaseNow `json:"databases"`
}

type databaseNow struct {
	Name           string  `json:"name"`
	Project        *string `json:"project"` // nil when no compose project matches
	SizeBytes      int64   `json:"size_bytes"`
	Connections    int     `json:"connections"`
	Active         int     `json:"active"`
	OldestXactSecs *int64  `json:"oldest_xact_secs"`
}

type databaseSeries struct {
	Name   string          `json:"name"`
	Points []databasePoint `json:"points"` // only buckets with data
}

type databasePoint struct {
	T           time.Time `json:"t"`
	Minutes     int       `json:"minutes"`
	Connections peak      `json:"connections"`
	ActiveMax   int       `json:"active_max"`
	SizeBytes   int64     `json:"size_bytes"` // the last reading in the bucket
}

func buildDatabases(now time.Time, name string, r historyRange, snap *snapshot.Snapshot, buckets []store.DatabaseBucket) databasesAnswer {
	from, to := historySpan(now, r)
	answer := databasesAnswer{Range: name, From: from, To: to, BucketSeconds: int(r.bucket / time.Second), GeneratedAt: now.UTC(),
		Databases: []databaseSeries{}}
	if snap != nil && snap.Databases != nil {
		d := snap.Databases
		answer.Now = &databaseServer{ReadAt: d.ReadAt.UTC(), MaxConnections: d.MaxConnections, Connections: d.Connections,
			Databases: []databaseNow{}}
		for _, db := range d.Databases {
			answer.Now.Databases = append(answer.Now.Databases, databaseNow{Name: db.Name, Project: optString(db.Project),
				SizeBytes: db.SizeBytes, Connections: db.Connections, Active: db.Active, OldestXactSecs: db.OldestXactSecs})
		}
	}
	for _, b := range buckets {
		n := len(answer.Databases)
		if n == 0 || answer.Databases[n-1].Name != b.Database {
			answer.Databases = append(answer.Databases, databaseSeries{Name: b.Database, Points: []databasePoint{}})
			n++
		}
		answer.Databases[n-1].Points = append(answer.Databases[n-1].Points, databasePoint{T: b.Start, Minutes: b.Minutes,
			Connections: peak{Avg: round(b.ConnAvg, 2), Max: float64(b.ConnMax)}, ActiveMax: b.ActiveMax, SizeBytes: b.SizeBytes})
	}
	return answer
}
