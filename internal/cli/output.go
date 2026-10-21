package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Data goes to stdout; messages go to stderr with a symbol (✓ ! ✗ ?), colored only on a terminal without NO_COLOR.
var (
	stdout   io.Writer = os.Stdout
	stderr   io.Writer = os.Stderr
	useColor           = colorEnabled()
)

const (
	yellow = "\033[33m"
	red    = "\033[31m"
	reset  = "\033[0m"
)

func colorEnabled() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}

func colorize(color string, s string) string {
	if !useColor {
		return s
	}
	return color + s + reset
}

// out writes data to stdout, followed by a newline.
func out(s string) {
	fmt.Fprintln(stdout, s)
}

// warn reports something the user should notice or fix, e.g. wrong arguments.
func warn(msg string) {
	fmt.Fprintln(stderr, colorize(yellow, "! "+msg))
}

// fail reports an error.
func fail(msg string) {
	fmt.Fprintln(stderr, colorize(red, "✗ "+msg))
}

// promptFor returns the prompt for an environment: "sonar (dev)> ", or
// "sonar (prod)> " with the label in red.
func promptFor(env string) string {
	env = strings.ToLower(strings.TrimSpace(env))
	if env == "" {
		return "sonar> "
	}
	label := "(" + env + ")"
	if env == "prod" {
		label = colorize(red, label)
	}
	return "sonar " + label + "> "
}
