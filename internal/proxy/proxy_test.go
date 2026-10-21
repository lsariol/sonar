package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/containers"
)

var (
	idA = strings.Repeat("a", 64) // plop: compose, healthy, public URL, CPU and memory limits
	idB = strings.Repeat("b", 64) // a `docker compose run` leftover
	idC = strings.Repeat("c", 64) // no compose labels, exited
	idD = strings.Repeat("d", 64) // lys: compose, no healthcheck, CPU quota
)

// fakeDocker serves the testdata answers like Docker's API and records every
// call it gets.
type fakeDocker struct {
	mu        sync.Mutex
	calls     []string
	failStats map[string]bool
	down      bool
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	down, failStats := f.down, f.failStats
	f.mu.Unlock()

	if down {
		http.Error(w, "daemon down", http.StatusServiceUnavailable)
		return
	}
	path := r.URL.Path
	switch {
	case path == "/_ping":
		io.WriteString(w, "OK")
	case path == "/containers/json":
		serveFile(w, "list.json")
	case strings.HasSuffix(path, "/json"):
		serveFile(w, "inspect-"+strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")+".json")
	case strings.HasSuffix(path, "/stats"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/stats")
		if failStats[id] {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		serveFile(w, "stats-"+id+".json")
	default:
		http.Error(w, "unexpected call", http.StatusTeapot)
	}
}

func serveFile(w http.ResponseWriter, name string) {
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// setup returns the fake daemon and a proxy server in front of it.
func setup(t *testing.T) (*fakeDocker, *httptest.Server) {
	t.Helper()
	fake := &fakeDocker{}
	dockerSrv := httptest.NewServer(fake)
	t.Cleanup(dockerSrv.Close)

	s := &Server{
		docker: &docker{base: dockerSrv.URL, http: dockerSrv.Client()},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	proxySrv := httptest.NewServer(s.Handler())
	t.Cleanup(proxySrv.Close)
	return fake, proxySrv
}

// get fetches path from the proxy and returns the status and body.
func get(t *testing.T, srv *httptest.Server, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestContainers(t *testing.T) {
	_, srv := setup(t)
	status, body := get(t, srv, "GET", "/v1/containers")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var list []containers.Container
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d containers, want 3 (the run leftover skipped): %+v", len(list), list)
	}

	scratch, lys, plop := list[0], list[1], list[2]
	if scratch.ID != idC || lys.ID != idD || plop.ID != idA {
		t.Fatalf("order: %s %s %s, want sorted by project, service, name", scratch.Name, lys.Name, plop.Name)
	}

	started := time.Date(2026, 10, 21, 9, 0, 1, 500_000_000, time.UTC)
	if plop.Name != "plop" || plop.Project != "plop" || plop.Service != "plop" || plop.Image != "plop-plop" ||
		plop.State != "running" || plop.Health != "healthy" || plop.RestartCount != 2 ||
		plop.RestartPolicy != "unless-stopped" || plop.CPULimit != 1.5 || plop.MemoryLimit != 536870912 ||
		plop.PublicURL != "https://plop.mobasity.com" || plop.Version != "v1.4.0" ||
		plop.StartedAt == nil || !plop.StartedAt.Equal(started) || plop.FinishedAt != nil {
		t.Errorf("plop = %+v", plop)
	}
	if scratch.Project != "" || scratch.Service != "scratchpad" || scratch.Health != "none" ||
		scratch.RestartPolicy != "no" || scratch.ExitCode != 137 || scratch.FinishedAt == nil {
		t.Errorf("scratchpad = %+v", scratch)
	}
	if lys.CPULimit != 0.5 || lys.Health != "none" || lys.Version != "v2.0.1" || lys.PublicURL != "" {
		t.Errorf("lys = %+v", lys)
	}
}

func TestStats(t *testing.T) {
	_, srv := setup(t)
	status, body := get(t, srv, "GET", "/v1/stats")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var answer containers.StatsAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Failed) != 0 || len(answer.Stats) != 2 {
		t.Fatalf("answer = %+v, want stats for the 2 running containers", answer)
	}

	byID := map[string]containers.Stats{}
	for _, s := range answer.Stats {
		byID[s.ID] = s
	}
	want := map[string]containers.Stats{
		idA: {ID: idA, ReadAt: time.Date(2026, 10, 21, 12, 0, 0, 500_000_000, time.UTC), CPUNanos: 123456789000,
			MemoryBytes: 104857600 - 20971520, MemoryLimit: 536870912, NetRxBytes: 5500, NetTxBytes: 7300,
			BlkReadBytes: 1024, BlkWriteBytes: 2076, PIDs: 12},
		idD: {ID: idD, ReadAt: time.Date(2026, 10, 21, 12, 0, 0, 700_000_000, time.UTC), CPUNanos: 5000000000,
			MemoryBytes: 40000000, MemoryLimit: 8000000000, NetRxBytes: 100, NetTxBytes: 200,
			BlkReadBytes: 10, BlkWriteBytes: 20, PIDs: 5},
	}
	for id, w := range want {
		got := byID[id]
		if !got.ReadAt.Equal(w.ReadAt) {
			t.Errorf("%s ReadAt = %v, want %v", id[:4], got.ReadAt, w.ReadAt)
		}
		got.ReadAt, w.ReadAt = time.Time{}, time.Time{}
		if got != w {
			t.Errorf("%s stats = %+v\nwant %+v", id[:4], got, w)
		}
	}
}

func TestStatsReportsFailures(t *testing.T) {
	fake, srv := setup(t)
	fake.failStats = map[string]bool{idD: true}

	_, body := get(t, srv, "GET", "/v1/stats")
	var answer containers.StatsAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Stats) != 1 || answer.Stats[0].ID != idA {
		t.Errorf("stats = %+v, want only plop's", answer.Stats)
	}
	if len(answer.Failed) != 1 || answer.Failed[0].ID != idD || !strings.Contains(answer.Failed[0].Reason, "500") {
		t.Errorf("failed = %+v, want lys with the HTTP 500", answer.Failed)
	}
}

// TestNoSecretsLeak checks that nothing beyond Sonar's fields gets out: the
// testdata hides SECRET-MARKER in env, commands, mounts, labels and more.
func TestNoSecretsLeak(t *testing.T) {
	_, srv := setup(t)
	for _, path := range []string{"/v1/containers", "/v1/stats", "/healthz"} {
		_, body := get(t, srv, "GET", path)
		if strings.Contains(body, "SECRET-MARKER") {
			t.Errorf("%s leaks hidden data: %s", path, body)
		}
	}
}

// TestOnlyFixedDockerCalls sends requests that try to reach other Docker
// paths and checks that Docker only ever saw the proxy's own fixed calls.
func TestOnlyFixedDockerCalls(t *testing.T) {
	fake, srv := setup(t)

	refused := []struct{ method, path string }{
		{"POST", "/v1/containers"},
		{"DELETE", "/v1/stats"},
		{"GET", "/v1/containers/" + idA},
		{"GET", "/containers/json"},
		{"GET", "/containers/" + idA + "/exec"},
		{"GET", "/v1/containers/../../containers/" + idA + "/json"},
		{"GET", "/v1/%2e%2e/containers/json"},
		{"GET", "/_ping"},
		{"GET", "/"},
	}
	for _, r := range refused {
		if status, _ := get(t, srv, r.method, r.path); status == http.StatusOK {
			t.Errorf("%s %s answered 200", r.method, r.path)
		}
	}
	get(t, srv, "GET", "/v1/containers?all=0&filters=x&id=../../exec")
	get(t, srv, "GET", "/v1/stats?stream=true")
	get(t, srv, "GET", "/healthz?x=1")

	hex := `[0-9a-f]{64}`
	allowed := regexp.MustCompile(`^GET (/_ping|/containers/json\?all=1|/containers/` + hex + `/json|/containers/` + hex + `/stats\?stream=false&one-shot=true)$`)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) == 0 {
		t.Fatal("Docker got no calls at all")
	}
	for _, call := range fake.calls {
		if !allowed.MatchString(call) {
			t.Errorf("Docker got an unexpected call: %s", call)
		}
	}
}

func TestHealth(t *testing.T) {
	fake, srv := setup(t)
	if status, body := get(t, srv, "GET", "/healthz"); status != http.StatusOK {
		t.Errorf("healthy: status %d: %s", status, body)
	}

	fake.down = true
	status, body := get(t, srv, "GET", "/healthz")
	if status != http.StatusBadGateway || !strings.Contains(body, "Docker:") {
		t.Errorf("Docker down: status %d: %s, want 502 naming Docker", status, body)
	}
	if status, _ := get(t, srv, "GET", "/v1/containers"); status != http.StatusBadGateway {
		t.Errorf("containers with Docker down: status %d, want 502", status)
	}
}

// TestServeOnSocket runs the proxy on a real Unix socket and reads it with the
// agent's client.
func TestServeOnSocket(t *testing.T) {
	fake := &fakeDocker{}
	dockerSrv := httptest.NewServer(fake)
	t.Cleanup(dockerSrv.Close)
	s := &Server{
		docker: &docker{base: dockerSrv.URL, http: dockerSrv.Client()},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "p.sock")
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, socket) }()

	client := containers.NewClient(socket)
	deadline := time.Now().Add(5 * time.Second)
	for client.Healthy(context.Background()) != nil {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("proxy never answered on the socket: %v", <-done)
		}
		time.Sleep(20 * time.Millisecond)
	}

	list, err := client.Containers(context.Background())
	if err != nil || len(list) != 3 {
		t.Errorf("Containers = %d, %v; want 3", len(list), err)
	}
	stats, err := client.Stats(context.Background())
	if err != nil || len(stats.Stats) != 2 {
		t.Errorf("Stats = %+v, %v; want 2", stats, err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("socket left behind after shutdown: %v", err)
	}
}

func TestParseTime(t *testing.T) {
	if !parseTime("0001-01-01T00:00:00Z").IsZero() || !parseTime("").IsZero() || !parseTime("garbage").IsZero() {
		t.Error("Docker's zero time, empty and unreadable should all be zero")
	}
	if optionalTime("0001-01-01T00:00:00Z") != nil {
		t.Error("optionalTime of Docker's zero time should be nil")
	}
}
