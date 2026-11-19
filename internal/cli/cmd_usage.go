package cli

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lsariol/sonar/internal/api"
	"github.com/lsariol/sonar/internal/snapshot"
)

// maxStateDays is the longest `states` reaches back: the minutes are kept 30 days.
const maxStateDays = 30

// daysFlag reads --days from args: def when absent, an error naming form when not 1 to most.
func daysFlag(args []string, def, most int, form string) (int, []string, error) {
	value, rest, err := takeValue(args, "--days")
	if err != nil {
		return 0, nil, usageError{reason: err.Error() + ".", form: form}
	}
	if value == "" {
		return def, rest, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > most {
		return 0, nil, usageError{reason: fmt.Sprintf("--days must be a number from 1 to %d.", most), form: form}
	}
	return n, rest, nil
}

func (c *CLI) databases(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "databases"}
	}
	snap, err := c.snapshot()
	if err != nil {
		return err
	}
	d := snap.Databases
	if d == nil {
		return fmt.Errorf("sparkdb hasn't been read yet: \"status\" shows why.")
	}
	rows := [][]string{{"DATABASE", "PROJECT", "SIZE", "CONNECTIONS", "ACTIVE", "OLDEST TRANSACTION"}}
	list := slices.Clone(d.Databases)
	slices.SortFunc(list, func(a, b snapshot.Database) int { return cmp.Compare(b.SizeBytes, a.SizeBytes) })
	for _, db := range list {
		oldest := "-"
		if db.OldestXactSecs != nil {
			oldest = duration(time.Duration(*db.OldestXactSecs) * time.Second)
		}
		rows = append(rows, []string{db.Name, orDash(db.Project), size(uint64(max(db.SizeBytes, 0))),
			strconv.Itoa(db.Connections), strconv.Itoa(db.Active), oldest})
	}
	table(rows)
	info(fmt.Sprintf("%d %s; sparkdb has %d of its %d connections in use (read %s).", len(list),
		plural(len(list), "database", "databases"), d.Connections, d.MaxConnections, formatTime(d.ReadAt)))
	return nil
}

func (c *CLI) disk(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "disk"}
	}
	snap, err := c.snapshot()
	if err != nil {
		return err
	}
	u := snap.Storage
	if u == nil {
		return fmt.Errorf("Disk use hasn't been measured yet: sonar-proxy measures a minute after it starts, then every 30 minutes.")
	}
	total := func(p snapshot.ProjectDisk) int64 {
		t := p.ImageBytes + p.ContainerBytes + p.VolumeBytes
		if p.DataBytes != nil {
			t += *p.DataBytes
		}
		return t
	}
	list := slices.Clone(u.Projects)
	slices.SortFunc(list, func(a, b snapshot.ProjectDisk) int { return cmp.Compare(total(b), total(a)) })
	rows := [][]string{{"PROJECT", "DATA FOLDER", "IMAGES", "CONTAINERS", "VOLUMES", "TOTAL"}}
	var short []string
	for _, p := range list {
		data := "-"
		if p.DataBytes != nil {
			data = size(uint64(*p.DataBytes))
		}
		if p.DataUnreadable > 0 {
			data += " (short)"
			short = append(short, fmt.Sprintf("%q (%d entries)", p.Project, p.DataUnreadable))
		}
		rows = append(rows, []string{p.Project, data, size(uint64(p.ImageBytes)), size(uint64(p.ContainerBytes)),
			size(uint64(p.VolumeBytes)), size(uint64(total(p)))})
	}
	table(rows)
	data := "-"
	if u.Totals.Data != nil {
		data = size(uint64(*u.Totals.Data))
	}
	info(fmt.Sprintf("%d %s. In all: data folders %s, images %s, containers %s, volumes %s, build cache %s. Measured %s in %s.",
		len(list), plural(len(list), "project", "projects"), data, size(uint64(u.Totals.Images)), size(uint64(u.Totals.Containers)),
		size(uint64(u.Totals.Volumes)), size(uint64(u.Totals.BuildCache)), formatTime(u.MeasuredAt),
		duration(time.Duration(u.Seconds*float64(time.Second)).Round(time.Second))))
	if len(short) > 0 {
		warn("Some entries couldn't be read, so these folders are larger than shown: " + strings.Join(short, ", ") + ". \"status\" says why.")
	}
	return nil
}

func (c *CLI) traffic(ctx context.Context, args []string) error {
	form := "traffic [--days N]"
	n, rest, err := daysFlag(args[1:], 7, maxDays, form)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usageError{form: form}
	}
	db, err := c.database(ctx)
	if err != nil {
		return err
	}
	now := c.opts.Now().UTC()
	to := now.Truncate(time.Hour).Add(time.Hour)
	buckets, err := db.TrafficHistory(ctx, to.Add(-time.Duration(n)*24*time.Hour), to, time.Duration(n)*24*time.Hour)
	if err != nil {
		return err
	}
	type site struct {
		host                    string
		requests, errors, bytes int64
	}
	byHost := make(map[string]*site)
	for _, b := range buckets {
		s := byHost[b.Host]
		if s == nil {
			s = &site{host: b.Host}
			byHost[b.Host] = s
		}
		s.requests, s.errors, s.bytes = s.requests+b.Requests, s.errors+b.Errors, s.bytes+b.Bytes
	}
	if len(byHost) == 0 {
		info(fmt.Sprintf("No traffic from Cloudflare in the last %d %s. \"status\" shows whether Sonar can read it.", n, plural(n, "day", "days")))
		return nil
	}
	projectOf := c.projectsByHost()
	list := make([]*site, 0, len(byHost))
	for _, s := range byHost {
		list = append(list, s)
	}
	slices.SortFunc(list, func(a, b *site) int { return cmp.Or(cmp.Compare(b.requests, a.requests), cmp.Compare(a.host, b.host)) })
	rows := [][]string{{"SITE", "PROJECT", "REQUESTS", "5XX", "SENT"}}
	for _, s := range list {
		rows = append(rows, []string{s.host, orDash(projectOf[s.host]), strconv.FormatInt(s.requests, 10),
			strconv.FormatInt(s.errors, 10), size(uint64(s.bytes))})
	}
	table(rows)
	info(fmt.Sprintf("%d %s, visitors' requests over the last %d %s, as Cloudflare counts them.",
		len(list), plural(len(list), "site", "sites"), n, plural(n, "day", "days")))
	return nil
}

// projectsByHost maps each checked hostname to its project, from the snapshot.
func (c *CLI) projectsByHost() map[string]string {
	out := make(map[string]string)
	snap, err := c.snapshot()
	if err != nil {
		return out
	}
	for _, ch := range snap.Checks {
		if u, err := url.Parse(ch.URL); err == nil && ch.Project != "" {
			out[strings.ToLower(u.Hostname())] = ch.Project
		}
	}
	return out
}

func (c *CLI) apps(ctx context.Context, args []string) error {
	form := "apps [project] [--days N]"
	n, rest, err := daysFlag(args[1:], 7, maxDays, form)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usageError{form: form}
	}
	only := ""
	if len(rest) == 1 {
		only = rest[0]
	}
	snap, err := c.snapshot()
	if err != nil {
		return err
	}
	db, err := c.database(ctx)
	if err != nil {
		return err
	}
	now := c.opts.Now()
	today := midnightUTC(now)
	days, err := db.AppCounterDays(ctx, today.AddDate(0, 0, -(n-1)), now)
	if err != nil {
		return err
	}

	var reporters []snapshot.AppReport
	for _, r := range snap.Apps {
		if only == "" || r.Project == only {
			reporters = append(reporters, r)
		}
	}
	type key struct{ project, name string }
	todays, totals := make(map[key]int64), make(map[key]int64)
	var keys []key
	for _, d := range days {
		if only != "" && d.Project != only {
			continue
		}
		k := key{d.Project, d.Name}
		if _, ok := totals[k]; !ok {
			keys = append(keys, k)
		}
		totals[k] += d.Total
		if d.Day.Equal(today) {
			todays[k] += d.Total
		}
	}
	if len(reporters) == 0 && len(keys) == 0 {
		scope := "No app serves a report"
		if only != "" {
			scope = fmt.Sprintf("%q serves no report", only)
		}
		info(scope + ": an app sets the com.mobasity.report-port label to serve one (README, \"App reports\").")
		return nil
	}

	if len(keys) > 0 {
		slices.SortFunc(keys, func(a, b key) int { return cmp.Or(cmp.Compare(a.project, b.project), cmp.Compare(a.name, b.name)) })
		rows := [][]string{{"PROJECT", "COUNTER", "TODAY (UTC)", fmt.Sprintf("LAST %d %s", n, strings.ToUpper(plural(n, "day", "days")))}}
		for _, k := range keys {
			rows = append(rows, []string{k.project, k.name, strconv.FormatInt(todays[k], 10), strconv.FormatInt(totals[k], 10)})
		}
		table(rows)
	}
	checks := 0
	var failing, unread []string
	rows := [][]string{{"PROJECT", "SERVICE", "CHECK", "STATE", "SINCE", "MESSAGE"}}
	for _, r := range reporters {
		if r.Error != "" {
			unread = append(unread, fmt.Sprintf("%s/%s: %s", r.Project, r.Service, r.Error))
		}
		for _, ch := range r.Checks {
			st := "ok"
			if !ch.OK {
				st = "failing"
				failing = append(failing, fmt.Sprintf("%s/%s", r.Project, ch.Name))
			}
			rows = append(rows, []string{r.Project, r.Service, ch.Name, st, formatTime(ch.Since), orDash(ch.Message)})
			checks++
		}
	}
	if checks > 0 {
		if len(keys) > 0 {
			out("")
		}
		table(rows)
	}
	info(fmt.Sprintf("%d %s, %d %s, %d %s.", len(reporters), plural(len(reporters), "app reporting", "apps reporting"),
		len(keys), plural(len(keys), "counter", "counters"), checks, plural(checks, "check", "checks")))
	for _, u := range unread {
		warn("Couldn't read the report of " + u)
	}
	if len(failing) > 0 {
		warn("Failing: " + strings.Join(failing, ", ") + ".")
	}
	return nil
}

func (c *CLI) states(ctx context.Context, args []string) error {
	form := "states <project> [--days N]"
	n, rest, err := daysFlag(args[1:], 7, maxStateDays, form)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageError{form: form}
	}
	project := rest[0]
	db, err := c.database(ctx)
	if err != nil {
		return err
	}
	now := c.opts.Now().UTC()
	length := time.Duration(n) * 24 * time.Hour
	to := now.Truncate(time.Minute).Add(time.Minute)
	from := to.Add(-length)
	spans, err := db.StateSpans(ctx, from, to)
	if err != nil {
		return err
	}
	services, err := db.ServiceTroubles(ctx, from, to)
	if err != nil {
		return err
	}
	checks, err := db.CheckTroubles(ctx, from, to)
	if err != nil {
		return err
	}
	list := api.ProjectSpans(now, length, project, spans, services, checks)
	if len(list) == 0 {
		if !slices.Contains(c.projectNames(), project) {
			return fmt.Errorf("No project %q is running, and it has no history in the last %d days. \"projects\" lists what's running.", project, n)
		}
		info(fmt.Sprintf("No history of %q in the last %d %s.", project, n, plural(n, "day", "days")))
		return nil
	}
	rows := [][]string{{"FROM", "TO", "STATE", "LASTED", "NOT UP"}}
	bad := 0
	for _, sp := range list {
		var why []string
		for _, s := range sp.Services {
			why = append(why, fmt.Sprintf("%s %s %d min", s.Service, s.State, s.Minutes))
		}
		for _, ch := range sp.Checks {
			w := fmt.Sprintf("%s %s", ch.URL, ch.State)
			if ch.Failures > 0 {
				w += fmt.Sprintf(", %d failed", ch.Failures)
			}
			if ch.LastError != nil {
				w += ": " + *ch.LastError
			}
			why = append(why, w)
		}
		if sp.State != "operational" && sp.State != "unknown" {
			bad++
		}
		rows = append(rows, []string{formatTime(sp.Start), formatTime(sp.End), sp.State, duration(sp.End.Sub(sp.Start)),
			orDash(strings.Join(why, "; "))})
	}
	table(rows)
	info(fmt.Sprintf("%d %s of %q over the last %d %s, %d not operational; gaps over 5 minutes are unknown.",
		len(list), plural(len(list), "span", "spans"), project, n, plural(n, "day", "days"), bad))
	return nil
}
