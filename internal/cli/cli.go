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

	"golang.org/x/term"
)

// Options are the CLI's settings.
type Options struct {
	Env     string
	Version string
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

	quit bool
}

// New returns a CLI with every command.
func New(opts Options) *CLI {
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
