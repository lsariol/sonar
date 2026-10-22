package cli

import (
	"context"
	"fmt"
	"strings"
)

// command is one CLI command; run gets the full arguments, args[0] included.
type command struct {
	names []string

	group    string
	synopsis string
	summary  string

	usages   []usage
	flags    []flag
	examples []example

	run func(c *CLI, ctx context.Context, args []string) error

	complete func(c *CLI) []string
}

// usage is one way to call a command, e.g. "days <project>", and what it does.
type usage struct {
	form string
	help string
}

// flag is an option a command takes, e.g. "--days N".
type flag struct {
	name string
	help string
}

// example is a command line and a short note on what it does.
type example struct {
	line string
	note string
}

// usageError is returned when a command is called with the wrong arguments.
type usageError struct {
	reason string
	form   string
}

func (e usageError) Error() string {
	if e.reason == "" {
		return "Usage: " + e.form
	}
	return e.reason + " Usage: " + e.form
}

// Command groups, in the order `help` shows them; empty groups are left out.
const (
	groupOverview = "Overview"
	groupChecks   = "Checks"
	groupHistory  = "History"
	groupSetup    = "Setup"
)

var groupOrder = []string{groupOverview, groupChecks, groupHistory, groupSetup}

// commandTable lists every command.
func commandTable() []command {
	return []command{
		{
			names:   []string{"status", "st"},
			group:   groupOverview,
			summary: "Is Sonar itself healthy?",
			usages: []usage{
				{"status", "Show Sonar's version, how old the agent's snapshot is, the database, and each collector (host, filesystem, docker, stats, checks, database) with its last reading. Prints one \"Needs attention\" line and exits non-zero when something is wrong."},
			},
			examples: []example{{"status", "after a deploy, or when the status page looks wrong"}},
			run:      (*CLI).status,
		},
		{
			names:   []string{"projects", "ls"},
			group:   groupOverview,
			summary: "Every project: state, uptime, response time",
			usages: []usage{
				{"projects", "List every compose project on the server: its state and what it rests on (an outside check, or Docker alone), its services, its uptime over 30 days with how much of that was observed, its response time over the last hour, and whether the status page shows it."},
			},
			examples: []example{{"projects", "the whole server at a glance"}},
			run:      (*CLI).projects,
		},
		{
			names:    []string{"project", "p"},
			group:    groupOverview,
			synopsis: "<name>",
			summary:  "One project's services, containers and checks",
			usages: []usage{
				{"project <name>", "Show one project in detail: its state, uptime over 30 and 90 days, each service (state, CPU, memory, network), each container (state, health, version, image, start time, restarts) and its outside checks. Containers without a compose project are \"(none)\"."},
			},
			examples: []example{{"project plop", "why is plop degraded?"}, {"project sonar", "Sonar's own containers"}},
			run:      (*CLI).project,
			complete: (*CLI).projectNames,
		},
		{
			names:   []string{"host"},
			group:   groupOverview,
			summary: "The server's CPU, memory, load and disk",
			usages: []usage{
				{"host", "Show the host's latest figures: CPU (overall and per core), memory and swap in use, load averages, how long it has been up, and the disk containers are stored on."},
			},
			run: (*CLI).host,
		},
		{
			names:   []string{"checks"},
			group:   groupChecks,
			summary: "Public addresses checked from outside",
			usages: []usage{
				{"checks", "List every public address Sonar checks (from each service's com.mobasity.public-url label): its state, the last check's time, result and latency, and the last error. Labels Sonar refuses to check are listed with the reason."},
			},
			run: (*CLI).checks,
		},
		{
			names:    []string{"days"},
			group:    groupHistory,
			synopsis: "<project> [--days N]",
			summary:  "A project's day-by-day availability",
			usages: []usage{
				{"days <project>", "Show a project's last 30 days, oldest first, as the status page shows them: each UTC day's state (the worst one that lasted 5 minutes), its uptime, and how much of it was observed (\"no_data\" under half). Then its uptime over 30 and 90 days."},
			},
			flags:    []flag{{"--days N", "How many days, from 1 to 380 (the history kept). 30 unless given."}},
			examples: []example{{"days plop", "the last 30 days"}, {"days plop --days 90", "the strip on the status page"}},
			run:      (*CLI).days,
			complete: (*CLI).projectNames,
		},
		{
			names:    []string{"updates"},
			group:    groupHistory,
			synopsis: "[project] [--days N]",
			summary:  "Deploys Sonar has seen, with versions",
			usages: []usage{
				{"updates", "List the updates of every project in the last 7 days, oldest first: when each started, the service, the version before and after, how long it took to come up, and its outcome (ok, failed, or timed out after 2 minutes)."},
				{"updates <project>", "Only that project's updates."},
			},
			flags:    []flag{{"--days N", "How many days back, from 1 to 380. 7 unless given."}},
			examples: []example{{"updates", "this week's deploys"}, {"updates plop --days 30", "plop's last month"}},
			run:      (*CLI).updates,
			complete: (*CLI).projectNames,
		},
		{
			names:   []string{"consumers"},
			group:   groupSetup,
			summary: "Who may use the API, and their tokens",
			usages: []usage{
				{"consumers", "List the API's consumers: each one's Cove key, what it may read, and whether its token is set (never the token itself). Consumers are defined in Sonar's code; their tokens come from Cove."},
			},
			run: (*CLI).consumers,
		},
		{
			names:    []string{"migrate"},
			group:    groupSetup,
			synopsis: "status",
			summary:  "The database migrations and when each ran",
			usages: []usage{
				{"migrate status", "List this version's database migrations and when each was applied. sonar-agent applies them when it starts; one still pending means it couldn't."},
			},
			run:      (*CLI).migrate,
			complete: func(*CLI) []string { return []string{"status"} },
		},
		{
			names:   []string{"version"},
			group:   groupSetup,
			summary: "Sonar's version",
			usages: []usage{
				{"version", "Print the version this Sonar runs: the release (v0.1.0), or dev-<commit> for a local build."},
			},
			run: (*CLI).version,
		},
		{
			names:    []string{"help", "h"},
			group:    groupSetup,
			synopsis: "[command]",
			summary:  "This overview, or one command in detail",
			usages: []usage{
				{"help", "List every command."},
				{"help <command>", "Show one command in detail, with examples."},
			},
			examples: []example{{"help version", "everything about version"}},
			run:      (*CLI).help,
			complete: (*CLI).commandNames,
		},
		{
			names:   []string{"exit", "quit"},
			group:   groupSetup,
			summary: "Leave the shell (Sonar keeps running)",
			usages:  []usage{{"exit", "Leave the shell. Sonar keeps running."}},
			run:     (*CLI).exit,
		},
	}
}

func (c *CLI) version(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "version"}
	}
	out(c.opts.Version)
	return nil
}

// help shows the overview or one command in detail.
func (c *CLI) help(ctx context.Context, args []string) error {
	switch len(args) {
	case 1:
		out(c.overview())
		return nil
	case 2:
		cmd, ok := c.byName[strings.ToLower(args[1])]
		if !ok {
			return usageError{reason: fmt.Sprintf("Unknown command %q.", args[1]), form: "help [command]"}
		}
		out(commandHelp(*cmd))
		return nil
	default:
		return usageError{form: "help [command]"}
	}
}

// overview lists every command on one line, by group.
func (c *CLI) overview() string {
	type line struct{ left, right string }
	byGroup := make(map[string][]line)
	width := 0
	for _, cmd := range c.commands {
		left := strings.TrimSpace(cmd.names[0] + " " + cmd.synopsis)
		byGroup[cmd.group] = append(byGroup[cmd.group], line{left, cmd.summary})
		width = max(width, len(left))
	}

	var b strings.Builder
	for _, group := range groupOrder {
		lines := byGroup[group]
		if len(lines) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(group + ":\n")
		for _, l := range lines {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, l.left, l.right)
		}
	}
	b.WriteString("\nType \"help <command>\" for details and examples, e.g. \"help days\".")
	return b.String()
}

// helpWidth is the widest line help text wraps to.
const helpWidth = 80

// commandHelp is `help <command>`: a header, then usage, flags and examples.
func commandHelp(cmd command) string {
	var b strings.Builder

	header := cmd.names[0]
	if len(cmd.names) > 1 {
		header += " (" + strings.Join(cmd.names[1:], ", ") + ")"
	}
	b.WriteString(header + ": " + cmd.summary + "\n")

	var usages, flags, examples [][2]string
	for _, u := range cmd.usages {
		usages = append(usages, [2]string{u.form, u.help})
	}
	for _, f := range cmd.flags {
		flags = append(flags, [2]string{f.name, f.help})
	}
	for _, e := range cmd.examples {
		examples = append(examples, [2]string{e.line, e.note})
	}

	width := columnWidth(append(append([][2]string{}, usages...), flags...), 30)
	b.WriteString("\nUsage:\n" + columns(usages, width))
	if len(flags) > 0 {
		b.WriteString("\nFlags:\n" + columns(flags, width))
	}
	if len(examples) > 0 {
		b.WriteString("\nExamples:\n" + columns(examples, columnWidth(examples, 46)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// columnWidth is the width of the left column for rows: the longest left
// side, not counting those longer than maxLeft (they get their own line).
func columnWidth(rows [][2]string, maxLeft int) int {
	width := 0
	for _, r := range rows {
		if len(r[0]) <= maxLeft {
			width = max(width, len(r[0]))
		}
	}
	return width
}

// columns lays out rows as two columns: the left one indented by two spaces
// and width wide, the right one wrapped to helpWidth.
func columns(rows [][2]string, width int) string {
	col := 2 + width + 3
	pad := strings.Repeat(" ", col)

	var b strings.Builder
	for _, r := range rows {
		lines := wrap(r[1], helpWidth-col)
		if len(r[0]) > width {
			b.WriteString("  " + r[0] + "\n")
			for _, l := range lines {
				b.WriteString(pad + l + "\n")
			}
			continue
		}
		for i, l := range lines {
			left := ""
			if i == 0 {
				left = r[0]
			}
			fmt.Fprintf(&b, "  %-*s   %s\n", width, left, l)
		}
	}
	return b.String()
}

// wrap splits text into lines of at most width characters, at spaces.
func wrap(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	return append(lines, line)
}

func (c *CLI) exit(ctx context.Context, args []string) error {
	c.quit = true
	return nil
}
