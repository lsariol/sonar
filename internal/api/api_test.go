package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
	"github.com/lsariol/sonar/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

var (
	now   = time.Date(2026, 10, 21, 15, 4, 5, 0, time.UTC)
	token = strings.Repeat("t", 40)
)

type fakeReader struct {
	days      []store.DayCounts
	response  map[string]int
	buckets   []store.HostBucket
	err       error
	dayCalls  atomic.Int32
	histCalls atomic.Int32
}

func (f *fakeReader) ProjectDays(ctx context.Context, from, to time.Time) ([]store.DayCounts, error) {
	f.dayCalls.Add(1)
	return f.days, f.err
}

func (f *fakeReader) ResponseTimes(context.Context, time.Time) (map[string]int, error) {
	return f.response, f.err
}

func (f *fakeReader) HostHistory(ctx context.Context, from, to time.Time, bucket time.Duration) ([]store.HostBucket, error) {
	f.histCalls.Add(1)
	return f.buckets, f.err
}

func (f *fakeReader) CheckSchemaVersion(context.Context) error { return nil }
func (f *fakeReader) Ping(context.Context) error               { return f.err }

type fakeSnapshots struct {
	snap snapshot.Snapshot
	err  error
}

func (f *fakeSnapshots) Load() (snapshot.Snapshot, error) { return f.snap, f.err }

// private values that must never reach a public answer.
var private = []string{
	strings.Repeat("a", 64), "plop-plop", "v1.4.0", "https://plop.mobasity.com", "postgres:16.4",
	"sparkdb", "cloudflared", "secret-sidecar", "16621363200", "28.7", "e2e-scratch", "cron", "nightly",
}

func f(v float64) *float64 { return &v }

func testSnapshot(at time.Time) snapshot.Snapshot {
	cpu := 28.7
	started := at.Add(-time.Hour)
	mem := uint64(90 << 20)
	return snapshot.Snapshot{
		Format: snapshot.Format, GeneratedAt: at, Version: "v0.1.0",
		Host: &snapshot.Host{CPUPercent: &cpu, MemoryTotal: 16621363200, MemoryUsedPercent: 20.5},
		Projects: []snapshot.Project{
			{Project: "", State: state.Unknown, Evidence: state.ContainerOnly,
				Services: []snapshot.Service{{Service: "e2e-scratch"}}},
			{Project: "cloudflared", State: state.Up, Evidence: state.ContainerOnly},
			{Project: "cron", State: state.Unknown, Evidence: state.ContainerOnly,
				Services: []snapshot.Service{{Service: "nightly", Counted: false}}},
			{Project: "lys", State: state.Down, Evidence: state.ContainerOnly,
				Services: []snapshot.Service{{Service: "lys", State: state.Down, Counted: true}}},
			{Project: "plop", State: state.Up, Evidence: state.Checked, Services: []snapshot.Service{
				{Service: "plop", State: state.Up, Counted: true, Usage: snapshot.Usage{MemoryBytes: &mem},
					Containers: []snapshot.Container{{ID: strings.Repeat("a", 64), Name: "plop", Image: "plop-plop",
						Version: "v1.4.0", State: "running", Health: "healthy", StartedAt: &started}}},
				{Service: "secret-sidecar", State: state.Up, Counted: true}}},
			{Project: "sparkdb", State: state.Up, Evidence: state.ContainerOnly, Services: []snapshot.Service{
				{Service: "db", Containers: []snapshot.Container{{Image: "postgres:16.4"}}}}},
		},
		Checks: []snapshot.Check{{Project: "plop", Service: "plop", URL: "https://plop.mobasity.com", State: state.Up}},
	}
}

func day(project string, daysAgo, up, degraded, down int, latency *int) store.DayCounts {
	return store.DayCounts{Project: project, Day: midnight(now).AddDate(0, 0, -daysAgo), Up: up, Degraded: degraded, Down: down, LatencyMs: latency}
}

func ms(v int) *int { return &v }

func sha(s string) [sha256.Size]byte { return sha256.Sum256([]byte(s)) }

func testDays() []store.DayCounts {
	return []store.DayCounts{
		day("plop", 0, 900, 4, 0, ms(51)),   // today: 904 of 904 minutes so far
		day("plop", 1, 1440, 0, 0, ms(48)),  // a clean day
		day("plop", 2, 1400, 0, 40, ms(60)), // an outage
		day("plop", 3, 600, 0, 0, nil),      // under half covered: no data
		day("lys", 1, 1430, 10, 0, nil),     // slow for a while
		day("sparkdb", 1, 1440, 0, 0, nil),  // excluded: never shown
	}
}

func newTestServer(t *testing.T, r *fakeReader, snaps *fakeSnapshots) *Server {
	t.Helper()
	s, err := New(Options{
		Version:   "v0.1.0",
		Getenv:    func(k string) string { return map[string]string{"SHARED_SONAR_MOBASITY_WEB_TOKEN": token}[k] },
		Snapshots: snaps,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       func() time.Time { return now },
		OpenReader: func(context.Context) (Reader, error) {
			return r, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.connect(context.Background())
	return s
}

func do(t *testing.T, s *Server, method, path, auth string) (int, http.Header, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.Bytes()
}

// golden compares body with testdata/name, or rewrites it with -update.
func golden(t *testing.T, name string, body []byte) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	path := filepath.Join("testdata", name)
	if *update {
		os.MkdirAll("testdata", 0o755)
		os.WriteFile(path, pretty.Bytes(), 0o644)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/api -update to create it)", err)
	}
	if !bytes.Equal(bytes.TrimSpace(pretty.Bytes()), bytes.TrimSpace(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")))) {
		t.Errorf("%s differs from the golden file:\n%s", name, pretty.String())
	}
}

func TestCredentials(t *testing.T) {
	get := func(v string) func(string) string { return func(string) string { return v } }
	if _, err := loadCredentials(get(""), consumers); err == nil || !strings.Contains(err.Error(), "generate SHARED_SONAR_MOBASITY_WEB_TOKEN") {
		t.Errorf("missing token: err = %v, want how to create it", err)
	}
	if _, err := loadCredentials(get("short"), consumers); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Errorf("short token: err = %v", err)
	}
	creds, err := loadCredentials(get(token), consumers)
	if err != nil || len(creds) != 1 {
		t.Fatalf("creds = %v, %v", creds, err)
	}
	if c, ok := identify(creds, token); !ok || c.Name != "mobasity-web" {
		t.Errorf("identify(right token) = %v, %v", c, ok)
	}
	if _, ok := identify(creds, token+"x"); ok {
		t.Error("identify(wrong token) matched")
	}
}

func TestAuth(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, &fakeSnapshots{snap: testSnapshot(now)})
	cases := []struct {
		auth   string
		status int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer", http.StatusUnauthorized},
		{"Basic " + token, http.StatusUnauthorized},
		{"Bearer " + token + "x", http.StatusUnauthorized},
		{"Bearer " + token, http.StatusOK},
		{"bearer  " + token + " ", http.StatusOK},
	}
	for _, tc := range cases {
		status, h, body := do(t, s, "GET", "/v1/status", tc.auth)
		if status != tc.status {
			t.Errorf("Authorization %q: %d, want %d: %s", tc.auth, status, tc.status, body)
		}
		if status == http.StatusUnauthorized && !strings.HasPrefix(h.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("Authorization %q: no WWW-Authenticate header", tc.auth)
		}
	}

	dashboard := strings.Repeat("d", 40)
	s.creds = append(s.creds, credential{consumer: consumer{Name: "dashboard", Scopes: []scope{monitorRead}},
		digest: sha(dashboard)})
	if status, _, _ := do(t, s, "GET", "/v1/status", "Bearer "+dashboard); status != http.StatusForbidden {
		t.Errorf("a token without status:read: %d, want 403", status)
	}
}

// TestRefusalsDontFloodTheLog: a flood of bad tokens logs one line a minute.
func TestRefusalsDontFloodTheLog(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, &fakeSnapshots{snap: testSnapshot(now)})
	var log bytes.Buffer
	s.opts.Log = slog.New(slog.NewTextHandler(&log, nil))
	clock := now
	s.opts.Now = func() time.Time { return clock }

	for range 1000 {
		do(t, s, "GET", "/v1/status", "Bearer wrong-token")
	}
	if n := strings.Count(log.String(), "refused requests"); n != 1 {
		t.Fatalf("%d log lines for 1000 refusals in one minute, want 1:\n%s", n, log.String())
	}
	clock = clock.Add(time.Minute)
	do(t, s, "GET", "/v1/status", "Bearer wrong-token")
	if !strings.Contains(log.String(), "count=1000") {
		t.Errorf("the next minute's summary should count the 1000 since:\n%s", log.String())
	}
}

func TestStatusGolden(t *testing.T) {
	r := &fakeReader{days: testDays(), response: map[string]int{"plop": 51, "sparkdb": 2}}
	s := newTestServer(t, r, &fakeSnapshots{snap: testSnapshot(now.Add(-4 * time.Second))})
	status, h, body := do(t, s, "GET", "/v1/status?days=5", "Bearer "+token)
	if status != http.StatusOK || h.Get("Content-Type") != "application/json" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v: %s", status, h, body)
	}
	golden(t, "status.json", body)
}

// TestNothingPrivate checks that no internal value reaches a public answer,
// and that every key in it is one the design lists.
func TestNothingPrivate(t *testing.T) {
	r := &fakeReader{days: testDays(), response: map[string]int{"plop": 51}, buckets: testBuckets()}
	s := newTestServer(t, r, &fakeSnapshots{snap: testSnapshot(now)})
	allowed := []string{"generated_at", "fresh", "overall", "state", "uptime_30d", "uptime_90d", "pct", "coverage_pct",
		"projects", "project", "evidence", "response_ms", "days", "date", "uptime_pct",
		"range", "from", "to", "bucket_seconds", "points", "t", "cpu_pct", "mem_used_pct", "avg", "min", "max"}

	for _, path := range []string{"/v1/status", "/v1/status?days=380", "/v1/status/history?range=1h", "/healthz"} {
		_, _, body := do(t, s, "GET", path, "Bearer "+token)
		for _, p := range private {
			if bytes.Contains(body, []byte(p)) {
				t.Errorf("%s leaks %q", path, p)
			}
		}
		var v any
		json.Unmarshal(body, &v)
		for _, k := range keys(v) {
			if !slices.Contains(allowed, k) && !(path == "/healthz" && (k == "status" || k == "version")) {
				t.Errorf("%s has a key the design doesn't list: %q", path, k)
			}
		}
	}
}

func keys(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			out = append(append(out, k), keys(child)...)
		}
	case []any:
		for _, child := range x {
			out = append(out, keys(child)...)
		}
	}
	return out
}

func TestStaleSnapshot(t *testing.T) {
	r := &fakeReader{days: testDays()}
	s := newTestServer(t, r, &fakeSnapshots{snap: testSnapshot(now.Add(-time.Minute))})
	_, _, body := do(t, s, "GET", "/v1/status?days=2", "Bearer "+token)
	var a statusAnswer
	json.Unmarshal(body, &a)
	if a.Fresh || a.Overall.State != "unknown" || len(a.Projects) != 2 {
		t.Fatalf("stale: %s", body)
	}
	for _, p := range a.Projects {
		if p.State != "unknown" || len(p.Days) != 2 {
			t.Errorf("%s: state %s, %d days; want unknown, history kept", p.Project, p.State, len(p.Days))
		}
	}

	s = newTestServer(t, r, &fakeSnapshots{err: errors.New("snapshot: missing")})
	_, _, body = do(t, s, "GET", "/v1/status", "Bearer "+token)
	if !bytes.Contains(body, []byte(`"fresh":false`)) || !bytes.Contains(body, []byte(`"projects":[]`)) {
		t.Errorf("no snapshot: %s", body)
	}
}

func testBuckets() []store.HostBucket {
	from, _ := historySpan(now, historyRanges["1h"])
	return []store.HostBucket{
		{Start: from, Samples: 12, Expected: 12, CPUAvg: f(10), CPUMin: f(5), CPUMax: f(20),
			MemPctAvg: f(20.5), MemPctMin: f(20.4), MemMax: f(20.6)},
		{Start: from.Add(2 * time.Minute), Samples: 6, Expected: 12, MemPctAvg: f(21), MemPctMin: f(21), MemMax: f(21)},
		{Start: from.Add(59 * time.Minute), Samples: 1, Expected: 12, CPUAvg: f(33.333), CPUMin: f(33.333), CPUMax: f(33.333),
			MemPctAvg: f(22.123456), MemPctMin: f(22.1), MemMax: f(22.2)},
	}
}

func TestHistoryGolden(t *testing.T) {
	s := newTestServer(t, &fakeReader{buckets: testBuckets()}, &fakeSnapshots{snap: testSnapshot(now)})
	status, _, body := do(t, s, "GET", "/v1/status/history?range=1h", "Bearer "+token)
	if status != http.StatusOK {
		t.Fatalf("%d: %s", status, body)
	}
	golden(t, "history-1h.json", body)
}

func TestHistoryRanges(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, &fakeSnapshots{snap: testSnapshot(now)})
	for name, want := range map[string]int{"1h": 60, "24h": 288, "7d": 336, "30d": 360, "": 288} {
		_, _, body := do(t, s, "GET", "/v1/status/history?range="+name, "Bearer "+token)
		var a historyAnswer
		json.Unmarshal(body, &a)
		last := a.Points[len(a.Points)-1].T
		if len(a.Points) != want || !a.To.After(now) || last.After(now) || a.From != a.To.Add(-historyRanges[a.Range].length) {
			t.Errorf("range %q: %d points from %v to %v (last %v); want %d ending with the bucket holding now", name, len(a.Points), a.From, a.To, last, want)
		}
	}
}

func TestBadParameters(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, &fakeSnapshots{snap: testSnapshot(now)})
	for _, path := range []string{"/v1/status?days=0", "/v1/status?days=381", "/v1/status?days=x", "/v1/status/history?range=2h"} {
		if status, _, body := do(t, s, "GET", path, "Bearer "+token); status != http.StatusBadRequest || !bytes.Contains(body, []byte(`"error"`)) {
			t.Errorf("%s: %d %s, want 400 with an error", path, status, body)
		}
	}
	if status, _, _ := do(t, s, "POST", "/v1/status", "Bearer "+token); status != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d, want 405", status)
	}
	if status, _, body := do(t, s, "GET", "/v1/snapshot", "Bearer "+token); status != http.StatusNotFound || !bytes.Contains(body, []byte(`"error"`)) {
		t.Errorf("unknown path: %d %s, want a JSON 404", status, body)
	}
}

func TestCache(t *testing.T) {
	r := &fakeReader{days: testDays(), buckets: testBuckets()}
	s := newTestServer(t, r, &fakeSnapshots{snap: testSnapshot(now)})
	for range 5 {
		do(t, s, "GET", "/v1/status", "Bearer "+token)
		do(t, s, "GET", "/v1/status/history", "Bearer "+token)
	}
	do(t, s, "GET", "/v1/status?days=30", "Bearer "+token)
	if r.dayCalls.Load() != 2 || r.histCalls.Load() != 1 {
		t.Errorf("database calls: %d status, %d history; want 2 (two day counts) and 1", r.dayCalls.Load(), r.histCalls.Load())
	}

	later := now.Add(statusCacheFor + time.Second)
	s.opts.Now = func() time.Time { return later }
	do(t, s, "GET", "/v1/status", "Bearer "+token)
	if r.dayCalls.Load() != 3 {
		t.Errorf("after the cache expired: %d calls, want 3", r.dayCalls.Load())
	}
}

func TestDatabaseNotReady(t *testing.T) {
	s, err := New(Options{
		Version: "v0.1.0", Getenv: func(string) string { return token }, Snapshots: &fakeSnapshots{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OpenReader: func(context.Context) (Reader, error) {
			return nil, errors.New("database: can't reach the database at SONAR_DATABASE_READER_URL: dial tcp 10.0.0.159:5432")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _, body := do(t, s, "GET", "/healthz", "")
	if status != http.StatusServiceUnavailable || bytes.Contains(body, []byte("10.0.0.159")) {
		t.Errorf("healthz: %d %s; want 503 without details", status, body)
	}
	if status, _, _ := do(t, s, "GET", "/v1/status", "Bearer "+token); status != http.StatusServiceUnavailable {
		t.Errorf("status before the database: %d, want 503", status)
	}

	r := &fakeReader{err: errors.New("database: reading daily rows: timeout")}
	s = newTestServer(t, r, &fakeSnapshots{snap: testSnapshot(now)})
	if status, _, body := do(t, s, "GET", "/v1/status", "Bearer "+token); status != http.StatusServiceUnavailable || bytes.Contains(body, []byte("timeout")) {
		t.Errorf("a failing query: %d %s; want 503 without details", status, body)
	}
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, &fakeSnapshots{})
	status, _, body := do(t, s, "GET", "/healthz", "")
	if status != http.StatusOK || string(bytes.TrimSpace(body)) != `{"status":"ok","version":"v0.1.0"}` {
		t.Errorf("healthz: %d %s", status, body)
	}
}

func TestServe(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, &fakeSnapshots{snap: testSnapshot(now)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, "127.0.0.1:0") }()
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve = %v", err)
	}
}
