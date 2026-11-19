package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
	"github.com/lsariol/sonar/internal/store"
)

var monitorPaths = []string{"/v1/monitor/status", "/v1/monitor/projects", "/v1/monitor/conditions",
	"/v1/monitor/host", "/v1/monitor/usage", "/v1/monitor/states", "/v1/monitor/databases", "/v1/monitor/disk", "/v1/monitor/traffic", "/v1/monitor/apps"}

// TestMonitorNeedsScope: only a token with monitor:read gets owner detail.
func TestMonitorNeedsScope(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, &fakeSnapshots{snap: testSnapshot(now)})
	for _, path := range monitorPaths {
		if status, _, _ := do(t, s, "GET", path, "Bearer "+token); status != http.StatusForbidden {
			t.Errorf("%s with mobasity-web's token: %d, want 403", path, status)
		}
		if status, _, _ := do(t, s, "GET", path, ""); status != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d, want 401", path, status)
		}
		if status, _, body := do(t, s, "GET", path, "Bearer "+vistaToken); status != http.StatusOK {
			t.Errorf("%s with vista's token: %d: %s", path, status, body)
		}
		if status, _, _ := do(t, s, "POST", path, "Bearer "+vistaToken); status != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d, want 405", path, status)
		}
	}
	_, _, body := do(t, s, "GET", "/v1/nope", "Bearer "+vistaToken)
	if !strings.Contains(string(body), "/v1/monitor/states") {
		t.Errorf("404 answer doesn't list the endpoints: %s", body)
	}
}

func monitorSnapshot(at time.Time) snapshot.Snapshot {
	snap := testSnapshot(at)
	snap.Projects[1].Group = "infrastructure" // cloudflared
	cores := 0.25
	snap.Projects = append(snap.Projects, snapshot.Project{Project: "cove", Group: "infrastructure", State: state.Degraded,
		Evidence: state.ContainerOnly, Services: []snapshot.Service{{Service: "cove", State: state.Degraded, Counted: true,
			Usage:  snapshot.Usage{CPUCores: &cores},
			Update: &snapshot.ServiceUpdate{Since: at.Add(-20 * time.Second), From: "v1.0.0", To: "v1.1.0"},
			Containers: []snapshot.Container{{Name: "cove", Image: "cove:local", Version: "v1.0.0", State: "running",
				Health: "starting", RestartCount: 3, CreatedAt: at.Add(-time.Hour), MemoryLimit: 64 << 20,
				Usage: snapshot.Usage{CPUCores: &cores}}}}}})
	snap.Checks[0].LastAt = &at
	snap.Checks[0].LastOutcome, snap.Checks[0].LastStatus = "ok", 200
	snap.Host.CorePercents = []float64{10.123, 20.456}
	snap.Host.UptimeSeconds = 86400
	snap.Host.ReadAt = at
	snap.Host.Network = []snapshot.NetIf{{Name: "enp3s0", RxRate: f(1234.56), TxRate: f(99.94), RxBytes: 1e9, TxBytes: 2e9},
		{Name: "docker0"}}
	snap.Host.Disk = &snapshot.Disk{Total: 450e9, Used: 400e9, Available: 50e9, UsedPercent: 88.89, Inodes: 1000, InodesFree: 900}
	snap.Collectors = map[string]snapshot.Collector{
		"docker":   {State: snapshot.Available, LastOK: &at},
		"database": {State: snapshot.Unavailable, Error: "database: connection refused"},
	}
	snap.Conditions = []snapshot.Condition{
		{ID: "project", Subject: "cove", Level: "warning", Message: "cove (infrastructure) is degraded", Since: at.Add(-3 * time.Minute)},
		{ID: "disk", Level: "warning", Message: "The disk is 89% full: 50.0 GB free of 450.0 GB", Since: at.Add(-time.Hour)},
	}
	snap.Attention = []string{"cove/cove: label com.mobasity.public-url=\"https://cove\": must be a public hostname"}
	return snap
}

func TestMonitorProjectsGolden(t *testing.T) {
	changed := now.Add(-2 * time.Hour).Truncate(time.Minute)
	first := now.Add(-20 * 24 * time.Hour).Truncate(time.Minute)
	r := &fakeReader{runs: map[string]store.StateRun{
		"plop": {LastOther: &changed, First: &first}, // up since the minute after it last wasn't
		"lys":  {First: &first},                      // down for as long as the history goes
	}}
	s := newTestServer(t, r, &fakeSnapshots{snap: monitorSnapshot(now.Add(-4 * time.Second))})
	status, _, body := do(t, s, "GET", "/v1/monitor/projects", "Bearer "+vistaToken)
	if status != http.StatusOK {
		t.Fatalf("%d: %s", status, body)
	}
	golden(t, "monitor-projects.json", body)

	var answer monitorProjectsAnswer
	json.Unmarshal(body, &answer)
	for _, p := range answer.Projects {
		if p.Project == "" {
			t.Error("a container without a compose project is listed")
		}
		if p.Project == "plop" && (p.StateSince == nil || !p.StateSince.Equal(changed.Add(time.Minute)) || !p.StateSinceExact) {
			t.Errorf("plop since %v exact %v, want %s", p.StateSince, p.StateSinceExact, changed.Add(time.Minute))
		}
		if p.Project == "lys" && (p.StateSince == nil || !p.StateSince.Equal(first) || p.StateSinceExact) {
			t.Errorf("lys since %v exact %v, want at least %s", p.StateSince, p.StateSinceExact, first)
		}
	}
}

func TestMonitorProjectsStale(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, &fakeSnapshots{snap: monitorSnapshot(now.Add(-time.Minute))})
	_, _, body := do(t, s, "GET", "/v1/monitor/projects", "Bearer "+vistaToken)
	var answer monitorProjectsAnswer
	json.Unmarshal(body, &answer)
	if answer.Fresh || len(answer.Projects) == 0 {
		t.Fatalf("answer = %s", body)
	}
	for _, p := range answer.Projects {
		if p.State != "unknown" || p.StateSince != nil {
			t.Errorf("%s on a stale snapshot: %s since %v, want unknown", p.Project, p.State, p.StateSince)
		}
	}
}

func TestConditionsGolden(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, &fakeSnapshots{snap: monitorSnapshot(now.Add(-4 * time.Second))})
	_, _, body := do(t, s, "GET", "/v1/monitor/conditions", "Bearer "+vistaToken)
	golden(t, "monitor-conditions.json", body)
}

// TestConditionsWithoutDatabase: Spark's state answers without the database, and a stale agent is critical.
func TestConditionsWithoutDatabase(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, &fakeSnapshots{snap: monitorSnapshot(now.Add(-2 * time.Minute))})
	s.db = nil
	status, _, body := do(t, s, "GET", "/v1/monitor/conditions", "Bearer "+vistaToken)
	var answer conditionsAnswer
	json.Unmarshal(body, &answer)
	if status != http.StatusOK || answer.State != "critical" || answer.Conditions[0].ID != "agent" {
		t.Errorf("%d: %s", status, body)
	}

	s.opts.Snapshots = &fakeSnapshots{err: errors.New("no snapshot")}
	_, _, body = do(t, s, "GET", "/v1/monitor/conditions", "Bearer "+vistaToken)
	json.Unmarshal(body, &answer)
	if answer.State != "unknown" {
		t.Errorf("without a snapshot: %s", body)
	}

	healthy := testSnapshot(now)
	s.opts.Snapshots = &fakeSnapshots{snap: healthy}
	_, _, body = do(t, s, "GET", "/v1/monitor/conditions", "Bearer "+vistaToken)
	answer = conditionsAnswer{}
	json.Unmarshal(body, &answer)
	if answer.State != "ok" || len(answer.Conditions) != 0 {
		t.Errorf("healthy: %s", body)
	}
}

func TestMonitorHostGolden(t *testing.T) {
	start := now.Truncate(time.Minute).Add(-58 * time.Minute)
	mem, total, swap, disk, diskTotal := int64(4e9), int64(16e9), int64(1e8), int64(400e9), int64(450e9)
	r := &fakeReader{updates: testUpdates(), hostDetail: []store.HostDetailBucket{
		{HostBucket: store.HostBucket{Start: start, Samples: 12, Expected: 12, CPUAvg: f(10), CPUMin: f(5), CPUMax: f(20),
			MemPctAvg: f(25), MemPctMin: f(24), MemMax: f(26)},
			MemUsedAvg: &mem, MemTotal: &total, SwapUsedAvg: &swap, Load1Avg: f(0.5), Load1Max: f(1.25),
			DiskUsed: &disk, DiskTotal: &diskTotal},
	}, network: []store.NetBucket{
		{Interface: "docker0", Start: start, Minutes: 1, RxBytes: 10, TxBytes: 20},
		{Interface: "enp3s0", Start: start, Minutes: 1, RxBytes: 6000, TxBytes: 9000},
		{Interface: "enp3s0", Start: start.Add(time.Minute), Minutes: 1, RxBytes: 7000, TxBytes: 8000},
	}}
	s := newTestServer(t, r, &fakeSnapshots{snap: monitorSnapshot(now)})
	status, _, body := do(t, s, "GET", "/v1/monitor/host?range=1h", "Bearer "+vistaToken)
	if status != http.StatusOK {
		t.Fatalf("%d: %s", status, body)
	}
	golden(t, "monitor-host.json", body)
	if !strings.Contains(string(body), `"project":"sparkdb"`) {
		t.Error("the owner's host chart leaves out sparkdb's deploy")
	}
}

func TestUsageGolden(t *testing.T) {
	at := now.Truncate(time.Hour)
	mem, memMax := int64(80<<20), int64(96<<20)
	r := &fakeReader{usage: []store.UsageBucket{
		{Project: "", Service: "e2e-scratch", Start: at, Minutes: 1}, // never listed
		{Project: "plop", Service: "plop", Start: at.Add(-5 * time.Minute), Minutes: 5, CPUAvg: f(0.0123456), CPUMax: f(0.5),
			MemAvg: &mem, MemMax: &memMax, NetRx: 1000, NetTx: 2000, BlkRead: 0, BlkWrite: 4096},
		{Project: "plop", Service: "plop", Start: at, Minutes: 4},
		{Project: "sparkdb", Service: "db", Start: at, Minutes: 5, CPUAvg: f(0.1), CPUMax: f(0.2)},
	}}
	s := newTestServer(t, r, &fakeSnapshots{snap: monitorSnapshot(now)})
	_, _, body := do(t, s, "GET", "/v1/monitor/usage?range=24h", "Bearer "+vistaToken)
	golden(t, "monitor-usage.json", body)

	_, _, body = do(t, s, "GET", "/v1/monitor/usage?range=24h&project=sparkdb", "Bearer "+vistaToken)
	var answer usageAnswer
	json.Unmarshal(body, &answer)
	if len(answer.Services) != 1 || answer.Services[0].Project != "sparkdb" {
		t.Errorf("project=sparkdb: %s", body)
	}
	if status, _, _ := do(t, s, "GET", "/v1/monitor/usage?project="+strings.Repeat("x", 200), "Bearer "+vistaToken); status != http.StatusBadRequest {
		t.Errorf("a long project name: %d, want 400", status)
	}
}

// TestStates: short gaps join, long ones are unknown spans, bad spans name what wasn't up.
func TestStates(t *testing.T) {
	at := now.Truncate(time.Minute).Add(-3 * time.Hour)
	m := func(minutes int) time.Time { return at.Add(time.Duration(minutes) * time.Minute) }
	r := &fakeReader{
		spans: []store.Span{
			{Project: "plop", State: state.Up, Start: m(0), End: m(30), Minutes: 30},
			{Project: "plop", State: state.Up, Start: m(33), End: m(60), Minutes: 27}, // 3-minute gap: joined
			{Project: "plop", State: state.Down, Start: m(60), End: m(65), Minutes: 5},
			{Project: "plop", State: state.Up, Start: m(65), End: m(100), Minutes: 35},
			{Project: "plop", State: state.Up, Start: m(120), End: m(180), Minutes: 60}, // 20-minute gap: unknown
			{Project: "sparkdb", State: state.Updating, Start: m(10), End: m(11), Minutes: 1},
		},
		serviceTrouble: []store.ServiceTrouble{
			{Project: "plop", Service: "plop", Minute: m(60), State: state.Down},
			{Project: "plop", Service: "plop", Minute: m(61), State: state.Degraded},
			{Project: "plop", Service: "worker", Minute: m(62), State: state.Down},
			{Project: "plop", Service: "plop", Minute: m(70), State: state.Down}, // outside the span
			{Project: "sparkdb", Service: "db", Minute: m(10), State: state.Updating},
		},
		checkTrouble: []store.CheckTrouble{
			{Project: "plop", Service: "plop", URL: "https://plop.mobasity.com", Minute: m(60), State: state.Up, Failures: 1, LastError: "HTTP 502 Bad Gateway"},
			{Project: "plop", Service: "plop", URL: "https://plop.mobasity.com", Minute: m(61), State: state.Down, Failures: 2, LastError: "timed out after 5s"},
		},
	}
	s := newTestServer(t, r, &fakeSnapshots{snap: monitorSnapshot(now)})
	status, _, body := do(t, s, "GET", "/v1/monitor/states?range=24h", "Bearer "+vistaToken)
	if status != http.StatusOK {
		t.Fatalf("%d: %s", status, body)
	}
	golden(t, "monitor-states.json", body)

	var answer statesAnswer
	json.Unmarshal(body, &answer)
	var got []string
	for _, sp := range answer.Projects[0].Spans {
		got = append(got, sp.State)
	}
	if want := "operational down operational unknown operational"; strings.Join(got, " ") != want {
		t.Errorf("plop's spans = %v, want %s", got, want)
	}
	down := answer.Projects[0].Spans[1]
	if len(down.Services) != 2 || down.Services[0].Minutes != 2 || down.Services[0].State != "down" ||
		len(down.Checks) != 1 || down.Checks[0].Failures != 3 || *down.Checks[0].LastError != "timed out after 5s" {
		t.Errorf("the outage's causes = %+v %+v", down.Services, down.Checks)
	}
	if status, _, _ := do(t, s, "GET", "/v1/monitor/states?range=90d", "Bearer "+vistaToken); status != http.StatusBadRequest {
		t.Errorf("range=90d: %d, want 400 (the minutes are kept 30 days)", status)
	}
}

func TestDatabasesGolden(t *testing.T) {
	at := now.Truncate(time.Hour)
	r := &fakeReader{databases: []store.DatabaseBucket{
		{Database: "lys_db", Start: at, Minutes: 5, ConnAvg: 3.333333, ConnMax: 5, ActiveMax: 1, SizeBytes: 12 << 20},
		{Database: "sonar_db", Start: at.Add(-5 * time.Minute), Minutes: 5, ConnAvg: 6, ConnMax: 6, SizeBytes: 300 << 20},
		{Database: "sonar_db", Start: at, Minutes: 4, ConnAvg: 6.5, ConnMax: 7, ActiveMax: 2, SizeBytes: 301 << 20},
	}}
	snap := monitorSnapshot(now)
	oldest := int64(42)
	snap.Databases = &snapshot.DatabaseServer{ReadAt: now.Add(-20 * time.Second), MaxConnections: 100, Connections: 12,
		Databases: []snapshot.Database{
			{Name: "AdminDB", SizeBytes: 8 << 20},
			{Name: "lys_db", Project: "lys", SizeBytes: 12 << 20, Connections: 5, Active: 1, OldestXactSecs: &oldest},
		}}
	s := newTestServer(t, r, &fakeSnapshots{snap: snap})
	status, _, body := do(t, s, "GET", "/v1/monitor/databases?range=24h", "Bearer "+vistaToken)
	if status != http.StatusOK {
		t.Fatalf("%d: %s", status, body)
	}
	golden(t, "monitor-databases.json", body)

	s = newTestServer(t, &fakeReader{}, &fakeSnapshots{snap: testSnapshot(now)})
	_, _, body = do(t, s, "GET", "/v1/monitor/databases", "Bearer "+vistaToken)
	if !strings.Contains(string(body), `"now":null`) {
		t.Errorf("before sparkdb has been read: %s", body)
	}
}

func TestDiskGolden(t *testing.T) {
	day := now.Truncate(24 * time.Hour)
	data, more := int64(5<<30), int64(6<<30)
	r := &fakeReader{disk: []store.DiskPoint{
		{Project: "", Start: day, MeasuredAt: day.Add(14 * time.Hour), DataBytes: &more, ImageBytes: 9 << 30, BuildCacheBytes: 2 << 30},
		{Project: "lys", Start: day.AddDate(0, 0, -1), MeasuredAt: day.Add(-30 * time.Minute), DataBytes: &data, ImageBytes: 30 << 20},
		{Project: "lys", Start: day, MeasuredAt: day.Add(14 * time.Hour), DataBytes: &more, ImageBytes: 30 << 20, VolumeBytes: 1 << 20},
		{Project: "plop", Start: day, MeasuredAt: day.Add(14 * time.Hour), ImageBytes: 12 << 20, ContainerBytes: 4096},
	}}
	snap := monitorSnapshot(now)
	snap.Storage = &snapshot.DiskUse{MeasuredAt: now.Add(-10 * time.Minute), Seconds: 12.345,
		Totals: snapshot.DiskTotals{Data: &more, Images: 9 << 30, BuildCache: 2 << 30},
		Projects: []snapshot.ProjectDisk{
			{Project: "lys", DataBytes: &more, DataFiles: 1200, ImageBytes: 30 << 20, VolumeBytes: 1 << 20},
			{Project: "plop", ImageBytes: 12 << 20, ContainerBytes: 4096},
		}}
	s := newTestServer(t, r, &fakeSnapshots{snap: snap})
	status, _, body := do(t, s, "GET", "/v1/monitor/disk", "Bearer "+vistaToken)
	if status != http.StatusOK {
		t.Fatalf("%d: %s", status, body)
	}
	golden(t, "monitor-disk.json", body)
	if status, _, _ := do(t, s, "GET", "/v1/monitor/disk?range=24h", "Bearer "+vistaToken); status != http.StatusBadRequest {
		t.Errorf("range=24h: %d, want 400 (disk is measured every 30 minutes)", status)
	}
}

func TestTrafficGolden(t *testing.T) {
	day := now.Truncate(24 * time.Hour)
	r := &fakeReader{traffic: []store.TrafficBucket{
		{Host: "dibs.mobasity.com", Start: day, Requests: 40, Bytes: 1000},
		{Host: "plop.mobasity.com", Start: day.Add(-24 * time.Hour), Requests: 900, Errors: 2, Bytes: 5 << 20},
		{Host: "plop.mobasity.com", Start: day, Requests: 300, Errors: 1, Bytes: 2 << 20},
	}}
	s := newTestServer(t, r, &fakeSnapshots{snap: monitorSnapshot(now)})
	status, _, body := do(t, s, "GET", "/v1/monitor/traffic?range=30d", "Bearer "+vistaToken)
	if status != http.StatusOK {
		t.Fatalf("%d: %s", status, body)
	}
	golden(t, "monitor-traffic.json", body)
	var answer trafficAnswer
	json.Unmarshal(body, &answer)
	if len(answer.Hosts) != 2 || answer.Hosts[0].Project != nil || *answer.Hosts[1].Project != "plop" || answer.Hosts[1].Requests != 1200 {
		t.Errorf("hosts = %+v", answer.Hosts)
	}
	if status, _, _ := do(t, s, "GET", "/v1/monitor/traffic?range=1h", "Bearer "+vistaToken); status != http.StatusBadRequest {
		t.Errorf("range=1h: %d, want 400 (Cloudflare's counts are hourly)", status)
	}
}

func TestAppsGolden(t *testing.T) {
	today := midnight(now)
	r := &fakeReader{
		appCounters: []store.AppCounterDay{
			{Project: "dibs", Name: "games_completed", Day: today.AddDate(0, 0, -1), Total: 40},
			{Project: "dibs", Name: "games_completed", Day: today, Total: 12},
			{Project: "lys", Name: "commands_run", Day: today, Total: 300},
		},
		appChanges: []store.AppCheckChange{
			{Project: "lys", Service: "lys", Name: "twitch_chat", At: now.Add(-10 * time.Minute), OK: true, Message: "connected to 3 channels"},
			{Project: "lys", Service: "lys", Name: "twitch_chat", At: now.Add(-15 * time.Minute), OK: false, Message: "disconnected: EOF"},
		},
	}
	snap := monitorSnapshot(now)
	read, started := now.Add(-20*time.Second), now.Add(-time.Hour)
	snap.Apps = []snapshot.AppReport{
		{Project: "lys", Service: "lys", Container: "lys", ReadAt: &read, StartedAt: started, Counters: map[string]uint64{"commands_run": 9000},
			Checks: []snapshot.AppCheck{{Name: "twitch_chat", OK: true, Message: "connected to 3 channels", Since: now.Add(-10 * time.Minute)}}},
		{Project: "maroo", Service: "maroo", Container: "maroo", Error: "GET http://maroo:8080/report/v1: connection refused"},
	}
	s := newTestServer(t, r, &fakeSnapshots{snap: snap})
	status, _, body := do(t, s, "GET", "/v1/monitor/apps?days=3", "Bearer "+vistaToken)
	if status != http.StatusOK {
		t.Fatalf("%d: %s", status, body)
	}
	golden(t, "monitor-apps.json", body)
	var answer appsAnswer
	json.Unmarshal(body, &answer)
	if len(answer.Projects) != 3 || answer.Projects[0].Counters[0].Total != 52 || len(answer.Projects[0].Counters[0].Days) != 3 {
		t.Errorf("projects = %+v", answer.Projects)
	}
}
