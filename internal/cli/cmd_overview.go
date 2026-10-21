package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lsariol/sonar/internal/api"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/store"
)

// status prints Sonar's own health; it fails when something needs attention.
func (c *CLI) status(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "status"}
	}
	now := c.opts.Now()
	var problems []string
	rows := [][2]string{{"Version", orDash(c.opts.Version)}, {"Environment", orDash(c.opts.Env)}}

	snap, snapErr := c.snapshot()
	if snapErr != nil {
		rows = append(rows, [2]string{"Snapshot", "none"})
		problems = append(problems, snapErr.Error())
	} else {
		rows = append(rows, [2]string{"Snapshot", fmt.Sprintf("%s (%s old)", formatTime(snap.GeneratedAt), age(now, snap.GeneratedAt))})
		if snap.Version != c.opts.Version {
			rows = append(rows, [2]string{"Agent version", orDash(snap.Version)})
		}
		if !snap.Fresh(now) {
			problems = append(problems, fmt.Sprintf("the snapshot is %s old: is sonar-agent running? (docker logs sonar-agent)", age(now, snap.GeneratedAt)))
		}
		problems = append(problems, snap.Attention...)
	}

	db, err := c.database(ctx)
	switch {
	case err != nil:
		rows = append(rows, [2]string{"Database", "unreachable"})
		problems = append(problems, err.Error())
	default:
		dbState := "ok"
		if err := db.CheckSchemaVersion(ctx); err != nil {
			dbState = "schema missing migrations"
			problems = append(problems, err.Error())
		}
		rows = append(rows, [2]string{"Database", dbState})
	}
	fields(rows)

	if snapErr == nil {
		names := make([]string, 0, len(snap.Collectors))
		for name := range snap.Collectors {
			names = append(names, name)
		}
		sort.Strings(names)
		out("")
		t := [][]string{{"COLLECTOR", "STATE", "LAST READING"}}
		for _, name := range names {
			col := snap.Collectors[name]
			t = append(t, []string{name, col.State, formatTimePtr(col.LastOK)})
		}
		table(t)
	}

	if len(problems) > 0 {
		for i, p := range problems {
			problems[i] = strings.TrimSuffix(p, ".")
		}
		return errors.New("Needs attention: " + strings.Join(problems, "; ") + ".")
	}
	return nil
}

// history reads every project's daily rows for the last days days, with the
// last hour's response times. Without the database it returns the error.
func (c *CLI) history(ctx context.Context, now time.Time, days int) (map[string][]store.DayCounts, map[string]int, error) {
	db, err := c.database(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := db.ProjectDays(ctx, now.UTC().AddDate(0, 0, -(days-1)), now)
	if err != nil {
		return nil, nil, err
	}
	response, err := db.ResponseTimes(ctx, now.Add(-time.Hour))
	if err != nil {
		return nil, nil, err
	}
	byProject := make(map[string][]store.DayCounts)
	for _, r := range rows {
		byProject[r.Project] = append(byProject[r.Project], r)
	}
	return byProject, response, nil
}

// uptimeText is "99.97% (98.6% covered)", or "- (3.0% covered)" under half.
func uptimeText(u api.Uptime) string {
	if u.Pct == nil {
		return fmt.Sprintf("- (%s covered)", percent(u.CoveragePct))
	}
	return fmt.Sprintf("%.2f%% (%s covered)", *u.Pct, percent(u.CoveragePct))
}

// publicText says whether a project is on the status page, and why not.
func publicText(project string) string {
	switch {
	case project == "":
		return "no (no compose project)"
	case api.IsPublic(project):
		return "yes"
	}
	return "no (excluded)"
}

func projectName(p string) string {
	if p == "" {
		return "(none)"
	}
	return p
}

func (c *CLI) projects(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "projects"}
	}
	snap, err := c.snapshot()
	if err != nil {
		return err
	}
	now := c.opts.Now()
	days, response, histErr := c.history(ctx, now, 90)

	rows := [][]string{{"PROJECT", "STATE", "EVIDENCE", "SERVICES", "UPTIME 30 DAYS", "RESPONSE", "PUBLIC"}}
	for _, p := range snap.Projects {
		up, resp := "-", "-"
		if histErr == nil {
			_, up30, _ := api.ProjectDays(now, 30, days[p.Project])
			up = uptimeText(up30)
			if ms, ok := response[p.Project]; ok {
				resp = fmt.Sprintf("%d ms", ms)
			}
		}
		rows = append(rows, []string{projectName(p.Project), p.State.String(), string(p.Evidence),
			fmt.Sprint(len(p.Services)), up, resp, publicText(p.Project)})
	}
	table(rows)
	info(fmt.Sprintf("%d %s", len(snap.Projects), plural(len(snap.Projects), "project", "projects")))

	if !snap.Fresh(now) {
		warn(fmt.Sprintf("The snapshot is %s old, so every state is unknown: is sonar-agent running?", age(now, snap.GeneratedAt)))
	}
	if histErr != nil {
		warn("Uptime and response times need the database: " + histErr.Error())
	}
	return nil
}

func (c *CLI) project(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return usageError{form: "project <name>"}
	}
	snap, err := c.snapshot()
	if err != nil {
		return err
	}
	name := args[1]
	if name == "(none)" {
		name = ""
	}
	i := slices.IndexFunc(snap.Projects, func(p snapshot.Project) bool { return p.Project == name })
	if i < 0 {
		return fmt.Errorf("No project %q is running. \"projects\" lists them.", args[1])
	}
	p := snap.Projects[i]
	now := c.opts.Now()

	rows := [][2]string{
		{"Project", projectName(p.Project)},
		{"State", fmt.Sprintf("%s (evidence: %s)", p.State, p.Evidence)},
		{"Public", publicText(p.Project)},
	}
	days, response, histErr := c.history(ctx, now, 90)
	if histErr == nil {
		_, up30, up90 := api.ProjectDays(now, 90, days[p.Project])
		rows = append(rows, [2]string{"Uptime 30 days", uptimeText(up30)}, [2]string{"Uptime 90 days", uptimeText(up90)})
		if ms, ok := response[p.Project]; ok {
			rows = append(rows, [2]string{"Response", fmt.Sprintf("%d ms (average, last hour)", ms)})
		}
	}
	fields(rows)

	out("")
	svc := [][]string{{"SERVICE", "STATE", "COUNTED", "CONTAINERS", "CPU", "MEMORY", "NET IN", "NET OUT"}}
	ctr := [][]string{{"CONTAINER", "SERVICE", "STATE", "HEALTH", "VERSION", "IMAGE", "STARTED", "RESTARTS", "CPU", "MEMORY"}}
	for _, s := range p.Services {
		counted := "yes"
		if !s.Counted {
			counted = "no (a one-off job)"
		}
		svc = append(svc, []string{s.Service, s.State.String(), counted, fmt.Sprint(len(s.Containers)),
			cores(s.Usage.CPUCores), sizePtr(s.Usage.MemoryBytes), rateOf(s.Usage.NetRxRate), rateOf(s.Usage.NetTxRate)})
		for _, k := range s.Containers {
			ctr = append(ctr, []string{k.Name, s.Service, k.State, k.Health, orDash(k.Version), k.Image,
				formatTimePtr(k.StartedAt), fmt.Sprint(k.RestartCount), cores(k.Usage.CPUCores), sizePtr(k.Usage.MemoryBytes)})
		}
	}
	table(svc)
	out("")
	table(ctr)

	var checks [][]string
	for _, ch := range snap.Checks {
		if ch.Project == p.Project {
			checks = append(checks, checkRow(now, ch))
		}
	}
	if len(checks) > 0 {
		out("")
		table(append([][]string{checkHeader}, checks...))
	}

	if !snap.Fresh(now) {
		warn(fmt.Sprintf("The snapshot is %s old, so these states are unknown: is sonar-agent running?", age(now, snap.GeneratedAt)))
	}
	if histErr != nil {
		warn("Uptime needs the database: " + histErr.Error())
	}
	return nil
}

func (c *CLI) host(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "host"}
	}
	snap, err := c.snapshot()
	if err != nil {
		return err
	}
	h := snap.Host
	if h == nil {
		return errors.New("The host hasn't been read yet: \"status\" shows why.")
	}

	cpu := percentPtr(h.CPUPercent)
	if len(h.CorePercents) > 0 {
		var each []string
		for _, p := range h.CorePercents {
			each = append(each, fmt.Sprintf("%.0f%%", p))
		}
		cpu += fmt.Sprintf(" (%d cores: %s)", len(h.CorePercents), strings.Join(each, " "))
	}
	rows := [][2]string{
		{"Read", fmt.Sprintf("%s (%s ago)", formatTime(h.ReadAt), age(c.opts.Now(), h.ReadAt))},
		{"CPU", cpu},
		{"Memory", fmt.Sprintf("%s of %s used (%s)", size(h.MemoryUsed), size(h.MemoryTotal), percent(h.MemoryUsedPercent))},
		{"Swap", fmt.Sprintf("%s of %s used", size(h.SwapUsed), size(h.SwapTotal))},
		{"Load", fmt.Sprintf("%.2f, %.2f, %.2f (1, 5, 15 min)", h.Load1, h.Load5, h.Load15)},
		{"Up", duration(time.Duration(h.UptimeSeconds * float64(time.Second)))},
	}
	if d := h.Disk; d != nil {
		rows = append(rows,
			[2]string{"Disk", fmt.Sprintf("%s of %s used (%s), %s free", size(d.Used), size(d.Total), percent(d.UsedPercent), size(d.Available))},
			[2]string{"Inodes", fmt.Sprintf("%d of %d free", d.InodesFree, d.Inodes)})
	} else {
		rows = append(rows, [2]string{"Disk", "-"})
	}
	fields(rows)

	if !snap.Fresh(c.opts.Now()) {
		warn(fmt.Sprintf("The snapshot is %s old: is sonar-agent running?", age(c.opts.Now(), snap.GeneratedAt)))
	}
	return nil
}

// projectNames completes project names from the snapshot.
func (c *CLI) projectNames() []string {
	snap, err := c.snapshot()
	if err != nil {
		return nil
	}
	var names []string
	for _, p := range snap.Projects {
		names = append(names, projectName(p.Project))
	}
	sort.Strings(names)
	return names
}
