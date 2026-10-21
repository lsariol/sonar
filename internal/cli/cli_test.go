package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// capture runs fn with stdout and stderr going to buffers.
func capture(t *testing.T, fn func()) (string, string) {
	t.Helper()
	var o, e bytes.Buffer
	oldOut, oldErr, oldColor := stdout, stderr, useColor
	stdout, stderr, useColor = &o, &e, false
	t.Cleanup(func() { stdout, stderr, useColor = oldOut, oldErr, oldColor })
	fn()
	return o.String(), e.String()
}

func TestCommandTable(t *testing.T) {
	seen := make(map[string]bool)
	for _, cmd := range commandTable() {
		if len(cmd.names) == 0 || cmd.summary == "" || len(cmd.usages) == 0 || cmd.run == nil {
			t.Errorf("command %v is missing names, a summary, usages or run", cmd.names)
		}
		if !slices.Contains(groupOrder, cmd.group) {
			t.Errorf("command %v has unknown group %q", cmd.names, cmd.group)
		}
		for _, name := range cmd.names {
			if seen[name] {
				t.Errorf("name %q is used twice", name)
			}
			seen[name] = true
		}
	}
}

func TestHelpFitsEightyColumns(t *testing.T) {
	c := New(Options{})
	texts := []string{c.overview()}
	for _, cmd := range c.commands {
		texts = append(texts, commandHelp(cmd))
	}
	for _, text := range texts {
		for _, line := range strings.Split(text, "\n") {
			if len([]rune(line)) > helpWidth {
				t.Errorf("line over %d characters: %q", helpWidth, line)
			}
		}
	}
}

func TestOverviewGroups(t *testing.T) {
	o := New(Options{}).overview()
	last := -1
	for _, g := range groupOrder {
		i := strings.Index(o, g+":")
		if i < 0 || i < last {
			t.Errorf("group %q missing or out of order:\n%s", g, o)
		}
		last = i
	}

	c := New(Options{})
	c.commands = slices.DeleteFunc(c.commands, func(cmd command) bool { return cmd.group == groupChecks })
	if strings.Contains(c.overview(), groupChecks+":") {
		t.Error("an empty group should be left out")
	}
}

func TestExec(t *testing.T) {
	c := New(Options{Version: "v0.1.0"})
	ctx := context.Background()

	o, _ := capture(t, func() {
		if err := c.Exec(ctx, []string{"VERSION"}); err != nil {
			t.Fatal(err)
		}
	})
	if o != "v0.1.0\n" {
		t.Errorf("version printed %q", o)
	}

	o, _ = capture(t, func() {
		if err := c.Exec(ctx, []string{"help", "h"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.HasPrefix(o, "help (h): ") || !strings.Contains(o, "Usage:") {
		t.Errorf("help help printed:\n%s", o)
	}

	if err := c.Exec(ctx, []string{"nope"}); err == nil || !strings.Contains(err.Error(), `"help"`) {
		t.Errorf("unknown command: err = %v", err)
	}

	var usage usageError
	if err := c.Exec(ctx, []string{"help", "nope"}); !errors.As(err, &usage) {
		t.Errorf("help nope: err = %v, want a usage error", err)
	}
	if err := c.Exec(ctx, []string{"version", "x"}); !errors.As(err, &usage) {
		t.Errorf("version x: err = %v, want a usage error", err)
	}
}

func TestReport(t *testing.T) {
	_, e := capture(t, func() { report(usageError{form: "help [command]"}) })
	if e != "! Usage: help [command]\n" {
		t.Errorf("usage error printed %q", e)
	}
	_, e = capture(t, func() { report(errors.New("database: unreachable")) })
	if e != "✗ database: unreachable\n" {
		t.Errorf("error printed %q", e)
	}
}

func TestExitStopsTheShell(t *testing.T) {
	c := New(Options{})
	if err := c.Exec(context.Background(), []string{"exit"}); err != nil || !c.quit {
		t.Errorf("exit: err = %v, quit = %v", err, c.quit)
	}
}

func TestComplete(t *testing.T) {
	c := New(Options{})
	cases := []struct {
		line, want string
		ok         bool
	}{
		{"he", "help ", true},
		{"help ve", "help version ", true},
		{"version ", "", false},
		{"zz", "", false},
	}
	for _, tc := range cases {
		got, _, ok := c.complete(tc.line, len(tc.line), '\t')
		if ok != tc.ok || got != tc.want {
			t.Errorf("complete(%q) = %q, %v; want %q, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPrompt(t *testing.T) {
	old := useColor
	useColor = false
	t.Cleanup(func() { useColor = old })

	for env, want := range map[string]string{"dev": "sonar (dev)> ", "prod": "sonar (prod)> ", "": "sonar> "} {
		if got := promptFor(env); got != want {
			t.Errorf("promptFor(%q) = %q, want %q", env, got, want)
		}
	}
}
