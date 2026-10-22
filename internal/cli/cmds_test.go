package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/api"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
	"github.com/lsariol/sonar/internal/store"
)

var now = time.Date(2026, 10, 21, 15, 4, 5, 0, time.UTC)

// tokenState is what the fake consumer's token looks like; tests change it.
var tokenState = "present"

func withToken(t *testing.T, state string) {
	t.Helper()
	tokenState = state
	t.Cleanup(func() { tokenState = "present" })
}

type fakeDB struct {
	days       []store.DayCounts
	response   map[string]int
	migrations []store.Migration
	updates    []store.Update
	schemaErr  error
}

func (f *fakeDB) ProjectDays(context.Context, time.Time, time.Time) ([]store.DayCounts, error) {
	return f.days, nil
}
func (f *fakeDB) ResponseTimes(context.Context, time.Time) (map[string]int, error) {
	return f.response, nil
}
func (f *fakeDB) Migrations(context.Context) ([]store.Migration, error) { return f.migrations, nil }
func (f *fakeDB) UpdatesBetween(context.Context, time.Time, time.Time) ([]store.Update, error) {
	return f.updates, nil
}
func (f *fakeDB) CheckSchemaVersion(context.Context) error { return f.schemaErr }

func fp(v float64) *float64 { return &v }
func up(v uint64) *uint64   { return &v }

func testSnap(at time.Time) snapshot.Snapshot {
	started := at.Add(-3 * time.Hour)
	checked := at.Add(-10 * time.Second)
	latency := int64(51)
	ok := at.Add(-5 * time.Second)
	return snapshot.Snapshot{
		Format: snapshot.Format, GeneratedAt: at, Version: "v0.1.0",
		Collectors: map[string]snapshot.Collector{
			"host":   {State: snapshot.Available, LastOK: &ok},
			"docker": {State: snapshot.Available, LastOK: &ok},
		},
		Host: &snapshot.Host{ReadAt: at, CPUPercent: fp(12.3), CorePercents: []float64{10, 12, 14, 13},
			MemoryTotal: 16 << 30, MemoryUsed: 3 << 30, MemoryUsedPercent: 18.75, SwapTotal: 1 << 30, SwapUsed: 512 << 10,
			Load1: 0.43, Load5: 0.19, Load15: 0.12, UptimeSeconds: 6856871,
			Disk: &snapshot.Disk{Total: 250 << 30, Used: 30 << 30, Available: 200 << 30, UsedPercent: 13, Inodes: 100, InodesFree: 90}},
		Projects: []snapshot.Project{
			{Project: "plop", State: state.Up, Evidence: state.Checked, Services: []snapshot.Service{
				{Service: "plop", State: state.Up, Counted: true, Usage: snapshot.Usage{CPUCores: fp(0.5), MemoryBytes: up(80 << 20)},
					Containers: []snapshot.Container{{Name: "plop", Image: "plop-plop", Version: "v1.4.0", State: "running",
						Health: "healthy", StartedAt: &started, RestartCount: 2, Usage: snapshot.Usage{CPUCores: fp(0.5)}}}},
				{Service: "migrate", Counted: false}}},
			{Project: "sparkdb", State: state.Up, Evidence: state.ContainerOnly},
		},
		Checks: []snapshot.Check{{Project: "plop", Service: "plop", URL: "https://plop.mobasity.com", State: state.Up,
			LastAt: &checked, LastOutcome: "ok", LastStatus: 200, LastLatencyMs: &latency}},
	}
}

func testCLI(t *testing.T, snap *snapshot.Snapshot, db *fakeDB) *CLI {
	t.Helper()
	opts := Options{
		Env: "prod", Version: "v0.1.0", Now: func() time.Time { return now },
		Snapshot: func() (snapshot.Snapshot, error) {
			if snap == nil {
				return snapshot.Snapshot{}, errors.New("snapshot: /var/lib/sonar/snapshot.json doesn't exist yet: is sonar-agent running?")
			}
			return *snap, nil
		},
		Consumers: func() []api.ConsumerInfo {
			return []api.ConsumerInfo{{Name: "mobasity-web", Key: "SHARED_SONAR_MOBASITY_WEB_TOKEN", Scopes: []string{"status:read"}, Token: tokenState}}
		},
	}
	if db != nil {
		opts.OpenDB = func(context.Context) (DB, error) { return db, nil }
	} else {
		opts.OpenDB = func(context.Context) (DB, error) {
			return nil, errors.New("database: can't reach the database at SONAR_DATABASE_READER_URL")
		}
	}
	return New(opts)
}

func run(t *testing.T, c *CLI, line string) (string, string, error) {
	t.Helper()
	var err error
	o, e := capture(t, func() { err = c.Exec(context.Background(), strings.Fields(line)) })
	return o, e, err
}

func mustContain(t *testing.T, what, text string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(text, p) {
			t.Errorf("%s is missing %q:\n%s", what, p, text)
		}
	}
}

func goodDB() *fakeDB {
	d := func(ago, upMin, down int) store.DayCounts {
		return store.DayCounts{Project: "plop", Day: now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -ago), Up: upMin, Down: down}
	}
	applied := now.Add(-time.Hour)
	return &fakeDB{
		days:     []store.DayCounts{d(0, 904, 0), d(1, 1440, 0), d(2, 1400, 40)},
		response: map[string]int{"plop": 51},
		migrations: []store.Migration{{Version: 1, Name: "00001_initial", AppliedAt: &applied},
			{Version: 2, Name: "00002_role_grants", AppliedAt: &applied}},
	}
}

func TestStatusHealthy(t *testing.T) {
	snap := testSnap(now.Add(-3 * time.Second))
	o, _, err := run(t, testCLI(t, &snap, goodDB()), "status")
	if err != nil {
		t.Fatalf("healthy status returned %v", err)
	}
	mustContain(t, "status", o, "Version:", "v0.1.0", "Snapshot:", "(3 s old)", "Database:", "ok", "COLLECTOR", "docker", "available")
}

func TestStatusNeedsAttention(t *testing.T) {
	snap := testSnap(now.Add(-2 * time.Minute))
	snap.Attention = []string{"cove/cove: label com.mobasity.public-url=\"https://cove\" must be a public hostname"}
	db := goodDB()
	db.schemaErr = errors.New("database: the schema is at version 1, this Sonar needs 2.")
	_, _, err := run(t, testCLI(t, &snap, db), "status")
	if err == nil {
		t.Fatal("status with problems returned nil")
	}
	mustContain(t, "the error", err.Error(), "Needs attention: ", "the snapshot is 2 min old", "cove/cove", "needs 2")
	if strings.Count(err.Error(), "..") > 0 || !strings.HasSuffix(err.Error(), ".") {
		t.Errorf("one sentence ending in a single period: %q", err)
	}

	_, _, err = run(t, testCLI(t, nil, nil), "status")
	mustContain(t, "no snapshot, no database", err.Error(), "is sonar-agent running", "can't reach the database")
}

func TestProjects(t *testing.T) {
	snap := testSnap(now)
	o, e, err := run(t, testCLI(t, &snap, goodDB()), "projects")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "projects", o, "PROJECT", "plop", "up", "checked", "51 ms", "yes", "sparkdb", "no (excluded)")
	mustContain(t, "projects", e, "2 projects")

	_, e, err = run(t, testCLI(t, &snap, nil), "projects")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "projects without the database", e, "Uptime and response times need the database")
}

func TestProject(t *testing.T) {
	snap := testSnap(now)
	c := testCLI(t, &snap, goodDB())
	o, _, err := run(t, c, "project plop")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "project plop", o, "Project:", "plop", "up (evidence: checked)", "Uptime 30 days:", "Response:", "51 ms",
		"SERVICE", "migrate", "no (a one-off job)", "0.50 cores", "80.0 MiB", "CONTAINER", "v1.4.0", "plop-plop", "healthy",
		"https://plop.mobasity.com", "ok (200)")

	if _, _, err := run(t, c, "project nope"); err == nil || !strings.Contains(err.Error(), `"projects" lists them`) {
		t.Errorf("unknown project: %v", err)
	}
	var usage usageError
	if _, _, err := run(t, c, "project"); !errors.As(err, &usage) {
		t.Errorf("no name: %v, want a usage error", err)
	}
	if got := c.projectNames(); strings.Join(got, ",") != "plop,sparkdb" {
		t.Errorf("completion = %v", got)
	}
}

func TestHost(t *testing.T) {
	snap := testSnap(now)
	o, _, err := run(t, testCLI(t, &snap, nil), "host")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "host", o, "CPU:", "12.3% (4 cores: 10% 12% 14% 13%)", "3.0 GiB of 16.0 GiB used (18.8%)",
		"512 KiB of 1.0 GiB", "0.43, 0.19, 0.12", "79 days, 8 h", "30.0 GiB of 250 GiB used (13.0%), 200 GiB free")
}

func TestChecks(t *testing.T) {
	snap := testSnap(now)
	snap.Attention = []string{`cove/cove: label com.mobasity.public-url="https://cove" must be a public hostname`}
	snap.Checks[0].LastError = "HTTP 502 Bad Gateway"
	o, e, err := run(t, testCLI(t, &snap, nil), "checks")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "checks", o, "URL", "https://plop.mobasity.com", "51 ms")
	mustContain(t, "checks", e, "1 address", "HTTP 502", "Not checked: cove/cove")

	snap.Checks = nil
	_, e, _ = run(t, testCLI(t, &snap, nil), "checks")
	mustContain(t, "no checks", e, "com.mobasity.public-url=https://")
}

func TestDays(t *testing.T) {
	snap := testSnap(now)
	c := testCLI(t, &snap, goodDB())
	o, e, err := run(t, c, "days plop --days 3")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "days", o, "DATE (UTC)", "2026-10-19", "down", "97.2%", "2026-10-21", "operational")
	mustContain(t, "days", e, "3 days. Uptime:")

	if _, _, err := run(t, c, "days nope"); err == nil || !strings.Contains(err.Error(), `No project "nope" is running`) {
		t.Errorf("unknown project: %v", err)
	}
	o, _, err = run(t, c, "days sparkdb --days 2")
	if err != nil || strings.Count(o, "no_data") != 2 {
		t.Errorf("a running project without history yet: %v\n%s; want 2 no_data days", err, o)
	}
	for _, bad := range []string{"days", "days plop --days 0", "days plop --days 400", "days plop --days", "days plop extra"} {
		var usage usageError
		if _, _, err := run(t, c, bad); !errors.As(err, &usage) {
			t.Errorf("%q: %v, want a usage error", bad, err)
		}
	}
}

func TestStatusMissingToken(t *testing.T) {
	withToken(t, "missing")
	snap := testSnap(now)
	_, _, err := run(t, testCLI(t, &snap, goodDB()), "status")
	if err == nil {
		t.Fatal("status with a missing token returned nil")
	}
	mustContain(t, "the error", err.Error(), "mobasity-web's API token (SHARED_SONAR_MOBASITY_WEB_TOKEN) is missing", "generate SHARED_SONAR_MOBASITY_WEB_TOKEN 64")
}

func TestConsumersAndMigrations(t *testing.T) {
	withToken(t, "missing")
	snap := testSnap(now)
	c := testCLI(t, &snap, goodDB())
	o, e, err := run(t, c, "consumers")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "consumers", o, "mobasity-web", "SHARED_SONAR_MOBASITY_WEB_TOKEN", "status:read", "missing")
	mustContain(t, "consumers", e, "1 consumer", "generate SHARED_SONAR_MOBASITY_WEB_TOKEN 64")

	o, e, err = run(t, c, "migrate status")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "migrate status", o, "00001_initial", "00002_role_grants")
	mustContain(t, "migrate status", e, "2 migrations")

	db := goodDB()
	db.migrations[1].AppliedAt = nil
	_, e, _ = run(t, testCLI(t, &snap, db), "migrate status")
	mustContain(t, "a pending migration", e, "1 migration is pending")

	var usage usageError
	if _, _, err := run(t, c, "migrate up"); !errors.As(err, &usage) {
		t.Errorf("migrate up: %v, want a usage error (the CLI only reads)", err)
	}
}

// TestControlCharactersCleaned: nothing printed can carry a terminal control
// sequence, whatever comes from outside.
func TestControlCharactersCleaned(t *testing.T) {
	snap := testSnap(now)
	snap.Checks[0].LastError = "x509: certificate is valid for \x1b]0;owned\x07evil.example"
	snap.Projects[0].Services[0].Containers[0].Version = "v1\x1b[2J"
	c := testCLI(t, &snap, goodDB())
	for _, line := range []string{"checks", "project plop"} {
		o, e, _ := run(t, c, line)
		if strings.ContainsAny(o+e, "\x1b\x07") {
			t.Errorf("%s printed a control character:\n%q", line, o+e)
		}
	}
	if got := clean("a\x1b[31mb\nc\td"); got != "a?[31mb\nc?d" {
		t.Errorf("clean = %q", got)
	}
}

func TestLocalTimes(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("EDT", -4*3600)
	t.Cleanup(func() { time.Local = old })
	if got := formatTime(time.Date(2026, 10, 21, 15, 4, 0, 0, time.UTC)); got != "2026-10-21 11:04" {
		t.Errorf("formatTime = %q, want the local 11:04", got)
	}
}

func TestFormats(t *testing.T) {
	cases := map[string]string{
		size(512):                          "512 B",
		size(1536):                         "1.5 KiB",
		size(250 << 30):                    "250 GiB",
		duration(90 * time.Second):         "1 min",
		duration(26 * time.Hour):           "1 day, 2 h",
		cores(fp(1)):                       "1.00 core",
		cores(fp(0.25)):                    "0.25 cores",
		cores(nil):                         "-",
		rateOf(fp(2048)):                   "2.0 KiB/s",
		age(now, now.Add(-90*time.Minute)): "1 h",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestUpdates(t *testing.T) {
	snap := testSnap(now)
	db := goodDB()
	at := now.Add(-2 * time.Hour)
	db.updates = []store.Update{
		{Project: "plop", Service: "plop", Started: at, Ended: at.Add(34 * time.Second), From: "v1.4.0", To: "v1.5.0", Outcome: "ok"},
		{Project: "lys", Service: "lys", Started: at.Add(time.Hour), Ended: at.Add(time.Hour + 2*time.Minute), From: "v2.0.0", To: "v2.1.0", Outcome: "timed out"},
	}
	c := testCLI(t, &snap, db)

	o, e, err := run(t, c, "updates")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "updates", o, "STARTED", "plop", "v1.4.0", "v1.5.0", "34 s", "ok", "lys", "timed out")
	mustContain(t, "updates", e, "2 updates of any project in the last 7 days, 1 that didn't come up healthy")

	o, _, _ = run(t, c, "updates plop --days 30")
	if strings.Contains(o, "lys") || !strings.Contains(o, "v1.5.0") {
		t.Errorf("updates plop:\n%s", o)
	}
	_, e, _ = run(t, c, "updates sparkdb")
	mustContain(t, "no updates", e, `No updates of "sparkdb" in the last 7 days`)

	o, _, _ = run(t, c, "days plop --days 1")
	mustContain(t, "days with an update", o, "UPDATES", "1")
	o, e, _ = run(t, c, "project plop")
	mustContain(t, "project plop", o, "v1.4.0", "v1.5.0")
	mustContain(t, "project plop", e, "The last 1 of 1 updates")
}
