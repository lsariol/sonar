// Command sonar is Spark's read-only monitoring service. One binary runs as
// each part of Sonar or as its CLI, picked by the first argument.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works on an image without zoneinfo

	"github.com/lsariol/sonar/internal/cli"
	"github.com/lsariol/sonar/internal/config"
	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/proxy"
)

// roles are the parts of Sonar the compose file starts, one per container.
var roles = []string{"proxy", "agent", "api"}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage(os.Stdout)
		return
	}

	switch {
	case args[0] == "-h" || args[0] == "--help":
		printUsage(os.Stdout)
	case args[0] == "proxy":
		runProxy()
	case slices.Contains(roles, args[0]):
		fatal(notBuilt(args[0]))
	case args[0] == "health":
		os.Exit(runHealth(args[1:]))
	case args[0] == "shell":
		runShell()
	case args[0] == "--version":
		os.Exit(runCommand([]string{"version"}))
	default:
		os.Exit(runCommand(args))
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  sonar shell                     Open the CLI
  sonar <command> [args]          Run one CLI command and exit
  sonar proxy | agent | api       Run that part of Sonar (what the compose file starts)
  sonar health <proxy|agent|api>  Exit 0 if that part is healthy (the healthchecks)

In Docker: docker exec -it sonar-api /sonar shell
       or: docker exec sonar-api /sonar help
Run "sonar help" to list the CLI commands.
`)
}

// runProxy runs sonar-proxy until SIGINT or SIGTERM (docker stop).
func runProxy() {
	cfg := loadConfig()
	log := newLogger("proxy")
	log.Info("Sonar starting", "version", cfg.Version, "env", cfg.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := proxy.New(proxy.DockerSocket, log).Serve(ctx, containers.Socket); err != nil {
		fatal(err)
	}
	log.Info("Sonar stopped")
}

// runHealth is a container's healthcheck: 0 when the named part is healthy.
func runHealth(args []string) int {
	if len(args) != 1 || !slices.Contains(roles, args[0]) {
		fmt.Fprintln(os.Stderr, "! Usage: sonar health <proxy|agent|api>")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	switch args[0] {
	case "proxy":
		err = containers.NewClient(containers.Socket).Healthy(ctx)
	default:
		err = notBuilt(args[0])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "sonar health: %v\n", err)
		return 1
	}
	fmt.Println("healthy")
	return 0
}

// newLogger logs text lines to stderr with UTC timestamps, tagged with part.
func newLogger(part string) *slog.Logger {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	})
	return slog.New(h).With("part", part)
}

// runShell runs the interactive CLI until exit, end of input, or SIGTERM.
func runShell() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	cli.New(cliOptions(loadConfig())).Run(ctx)
}

// runCommand runs one CLI command and returns the exit status.
func runCommand(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.New(cliOptions(loadConfig())).Exec(ctx, args); err != nil {
		cli.Report(err)
		return 1
	}
	return 0
}

func cliOptions(cfg config.Config) cli.Options {
	return cli.Options{Env: cfg.Env, Version: cfg.Version}
}

// notBuilt is the error for a part of Sonar that doesn't exist yet.
func notBuilt(role string) error {
	return fmt.Errorf("the %s isn't built yet", role)
}

func loadConfig() config.Config {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fatal(err)
	}
	return cfg
}

// fatal prints an error and exits.
func fatal(err error) {
	fmt.Fprintf(os.Stderr, "sonar: %v\n", err)
	os.Exit(1)
}
