// Package proxy is sonar-proxy, the only part of Sonar with the Docker
// socket. It answers three fixed requests on a Unix socket and builds every
// Docker call itself: nothing a caller sends ever reaches Docker.
package proxy

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/lsariol/sonar/internal/containers"
)

const (
	// DockerSocket is where the compose file mounts Docker's socket.
	DockerSocket = "/var/run/docker.sock"

	// parallel is how many Docker calls the proxy makes at once.
	parallel = 8

	// statsTimeout bounds one container's one-shot stats.
	statsTimeout = 3 * time.Second
)

// Server answers the agent's three requests.
type Server struct {
	docker *docker
	log    *slog.Logger
}

// New returns a server that reads Docker through dockerSocket.
func New(dockerSocket string, log *slog.Logger) *Server {
	return &Server{docker: newDocker(dockerSocket), log: log}
}

// Handler routes the three requests; any other path is 404, any other method
// 405. Query strings and bodies are never read.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/containers", s.containers)
	mux.HandleFunc("GET /v1/stats", s.stats)
	mux.HandleFunc("GET /healthz", s.health)
	return mux
}

// Serve answers on a Unix socket at path until ctx is cancelled. A socket
// left by an earlier run is replaced.
func (s *Server) Serve(ctx context.Context, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("proxy: removing the old socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("proxy: listening on %s: %w: is the proxy volume mounted at its folder?", path, err)
	}
	defer os.Remove(path)
	// Only the proxy and the agent mount the folder, so anyone who can see
	// the socket may use it; the agent runs as another user.
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return fmt.Errorf("proxy: making %s usable by the agent: %w", path, err)
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	s.log.Info("proxy listening", "socket", path)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("proxy: %w", err)
	}
	return nil
}

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	list, err := s.docker.list(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}

	found := make([]*containers.Container, len(list))
	errs := make([]error, len(list))
	each(len(list), func(i int) {
		c, err := s.docker.inspect(r.Context(), list[i].ID)
		switch {
		case errors.Is(err, errNotFound):
			// Removed since the list: it no longer exists to report.
		case err != nil:
			errs[i] = err
		default:
			out := toContainer(c)
			found[i] = &out
		}
	})
	if err := errors.Join(errs...); err != nil {
		s.fail(w, err)
		return
	}

	answer := make([]containers.Container, 0, len(found))
	for _, c := range found {
		if c != nil {
			answer = append(answer, *c)
		}
	}
	slices.SortFunc(answer, func(a, b containers.Container) int {
		return cmp.Or(cmp.Compare(a.Project, b.Project), cmp.Compare(a.Service, b.Service), cmp.Compare(a.Name, b.Name))
	})
	writeJSON(w, http.StatusOK, answer)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	list, err := s.docker.list(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	var running []string
	for _, c := range list {
		if c.State == "running" {
			running = append(running, c.ID)
		}
	}

	results := make([]*containers.Stats, len(running))
	reasons := make([]string, len(running))
	each(len(running), func(i int) {
		ctx, cancel := context.WithTimeout(r.Context(), statsTimeout)
		defer cancel()
		raw, err := s.docker.stats(ctx, running[i])
		switch {
		case errors.Is(err, errNotFound):
			reasons[i] = "gone"
		case err != nil && ctx.Err() != nil:
			reasons[i] = "timed out"
		case err != nil:
			reasons[i] = err.Error()
		default:
			out := toStats(running[i], raw)
			results[i] = &out
		}
	})

	answer := containers.StatsAnswer{Stats: []containers.Stats{}, Failed: []containers.StatsError{}}
	for i, st := range results {
		if st != nil {
			answer.Stats = append(answer.Stats, *st)
		} else {
			answer.Failed = append(answer.Failed, containers.StatsError{ID: running[i], Reason: reasons[i]})
		}
	}
	writeJSON(w, http.StatusOK, answer)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.ping(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// fail logs err and answers 502 with it.
func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("Docker call failed", "err", err)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// each runs fn for 0..n-1, at most parallel at a time, and waits for all.
func each(n int, fn func(i int)) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, parallel)
	for i := range n {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			fn(i)
		}()
	}
	wg.Wait()
}
