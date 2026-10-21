// Package cli is Sonar's command line: the shell and one-shot commands. Every
// command is read-only.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/lsariol/sonar/internal/api"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/store"
)

// Options are the CLI's settings and where its data comes from.
type Options struct {
	Env     string
	Version string

	Snapshot  func() (snapshot.Snapshot, error)
	OpenDB    func(ctx context.Context) (DB, error) // the reader's connection
	Consumers func() []api.ConsumerInfo
	Now       func() time.Time // time.Now unless set
}

// DB is the history, as the CLI reads it (sonar_reader).
type DB interface {
	ProjectDays(ctx context.Context, from, to time.Time) ([]store.DayCounts, error)
	ResponseTimes(ctx context.Context, since time.Time) (map[string]int, error)
	Migrations(ctx context.Context) ([]store.Migration, error)
	CheckSchemaVersion(ctx context.Context) error
}

// CLI runs Sonar's commands, one at a time or as a shell.
type CLI struct {
	opts   Options
	prompt string

	scanner     *bufio.Scanner
	term        *term.Terminal
	interactive bool

	commands []command
	byName   map[string]*command

	db   DB // opened on the first command that needs it
	quit bool
}

// New returns a CLI with every command.
func New(opts Options) *CLI {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &CLI{
		opts:        opts,
		prompt:      promptFor(opts.Env),
		scanner:     bufio.NewScanner(os.Stdin),
		interactive: term.IsTerminal(int(os.Stdin.Fd())),
		commands:    commandTable(),
		byName:      make(map[string]*command),
	}
	for i := range c.commands {
		for _, name := range c.commands[i].names {
			c.byName[name] = &c.commands[i]
		}
	}
	return c
}

// Run reads and runs commands until stdin closes, `exit` is typed, or ctx is
// cancelled.
func (c *CLI) Run(ctx context.Context) {
	if c.interactive && c.runTerminal(ctx) {
		return
	}
	for ctx.Err() == nil && !c.quit {
		fmt.Fprint(stderr, c.prompt)
		if !c.scanner.Scan() {
			return
		}
		args := strings.Fields(c.scanner.Text())
		if len(args) == 0 {
			continue
		}
		report(c.Exec(ctx, args))
	}
}

// Exec runs a single command, e.g. ["help", "version"].
func (c *CLI) Exec(ctx context.Context, args []string) error {
	cmd, ok := c.byName[strings.ToLower(args[0])]
	if !ok {
		return fmt.Errorf("Unknown command %q. Type \"help\" to see the available commands.", args[0])
	}
	return cmd.run(c, ctx, args)
}

// Report shows an error returned by Exec: wrong arguments as a warning,
// anything else as an error.
func Report(err error) {
	report(err)
}

func report(err error) {
	if err == nil {
		return
	}
	var usage usageError
	if errors.As(err, &usage) {
		warn(err.Error())
		return
	}
	fail(err.Error())
}

// database returns the history's connection, opening it the first time.
func (c *CLI) database(ctx context.Context) (DB, error) {
	if c.db != nil {
		return c.db, nil
	}
	if c.opts.OpenDB == nil {
		return nil, errors.New("database: not available here: run the CLI in sonar-api (docker exec -it sonar-api /sonar shell)")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := c.opts.OpenDB(ctx)
	if err != nil {
		return nil, err
	}
	c.db = db
	return db, nil
}

// snapshot returns the agent's latest snapshot.
func (c *CLI) snapshot() (snapshot.Snapshot, error) {
	if c.opts.Snapshot == nil {
		return snapshot.Snapshot{}, errors.New("snapshot: not available here: run the CLI in sonar-api (docker exec -it sonar-api /sonar shell)")
	}
	return c.opts.Snapshot()
}
