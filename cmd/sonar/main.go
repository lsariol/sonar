// Command sonar is Spark's read-only monitoring service. One binary runs as
// each part of Sonar or as its CLI, picked by the first argument.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works on an image without zoneinfo

	"github.com/lsariol/sonar/internal/agent"
	"github.com/lsariol/sonar/internal/api"
	"github.com/lsariol/sonar/internal/checks"
	"github.com/lsariol/sonar/internal/cli"
	"github.com/lsariol/sonar/internal/config"
	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/host"
	"github.com/lsariol/sonar/internal/proxy"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/store"
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
	case args[0] == "agent":
		runAgent()
	case args[0] == "api":
		runAPI()
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

// runAgent runs sonar-agent until SIGINT or SIGTERM (docker stop).
func runAgent() {
	cfg := loadConfig()
	log := newLogger("agent")
	log.Info("Sonar starting", "version", cfg.Version, "env", cfg.Env)

	migratorURL, err := config.Secret(os.Getenv, "SONAR_DATABASE_MIGRATOR_URL")
	if err != nil {
		fatal(err)
	}
	appURL, err := config.Secret(os.Getenv, "SONAR_DATABASE_URL")
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := agent.New(agent.Options{
		Version:  cfg.Version,
		StateDir: snapshot.Dir,
		Host:     host.Reader{Root: "/"},
		DiskPath: "/",
		Docker:   containers.NewClient(containers.Socket),
		Checker:  checks.New(cfg.Version),
		Log:      log,
		OpenStore: func(ctx context.Context) (agent.Store, error) {
			if err := store.Migrate(ctx, migratorURL, log); err != nil {
				return nil, err
			}
			db, err := store.Connect(ctx, appURL, "SONAR_DATABASE_URL")
			if err != nil {
				return nil, err
			}
			if err := db.CheckSchemaVersion(ctx); err != nil {
				db.Close()
				return nil, err
			}
			log.Info("database ready")
			return db, nil
		},
	})
	if err := a.Run(ctx); err != nil {
		fatal(err)
	}
	log.Info("Sonar stopped")
}

// runAPI runs sonar-api until SIGINT or SIGTERM (docker stop).
func runAPI() {
	cfg := loadConfig()
	log := newLogger("api")
	log.Info("Sonar starting", "version", cfg.Version, "env", cfg.Env)

	readerURL, err := config.Secret(os.Getenv, "SONAR_DATABASE_READER_URL")
	if err != nil {
		fatal(err)
	}
	s, err := api.New(api.Options{
		Version:   cfg.Version,
		Getenv:    os.Getenv,
		Snapshots: snapshot.NewCache(snapshot.Dir),
		Log:       log,
		OpenReader: func(ctx context.Context) (api.Reader, error) {
			db, err := store.Connect(ctx, readerURL, "SONAR_DATABASE_READER_URL")
			if err != nil {
				return nil, err
			}
			return db, nil
		},
	})
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.Serve(ctx, fmt.Sprintf(":%d", api.Port)); err != nil {
		fatal(err)
	}
	log.Info("Sonar stopped")
}

// apiHealthy is nil when the API answers its /healthz with 200.
func apiHealthy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", api.Port), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("the API doesn't answer on port %d: %w", api.Port, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the API answers %s: it can't reach the database yet (see docker logs sonar-api)", resp.Status)
	}
	return nil
}

// agentHealthy is nil while the agent's snapshot is fresh.
func agentHealthy() error {
	s, err := snapshot.Read(snapshot.Dir)
	if err != nil {
		return err
	}
	if age := time.Since(s.GeneratedAt); !s.Fresh(time.Now()) {
		return fmt.Errorf("the snapshot is %s old (written %s): is sonar-agent stuck?", age.Round(time.Second), s.GeneratedAt.Format(time.RFC3339))
	}
	return nil
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
	case "agent":
		err = agentHealthy()
	case "api":
		err = apiHealthy(ctx)
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
