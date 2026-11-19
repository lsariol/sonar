package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/state"
)

// TestMonitorReads checks the owner's queries as the reader: runs, spans, trouble, usage, host.
func TestMonitorReads(t *testing.T) {
	app := open(t)
	ctx := context.Background()
	reader, err := Connect(ctx, readerURL, "SONAR_TEST_DATABASE_READER_URL")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	// plop: up 0-9, down 10-11, up 12-14, a gap at 15-16, up 17-19.
	base := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	for n := range 20 {
		if n == 15 || n == 16 {
			continue
		}
		m := sampleMinute(base.Add(time.Duration(n) * time.Minute))
		st := state.Up
		if n == 10 || n == 11 {
			st = state.Down
			m.Services[0].State = state.Down
			m.Checks[0].Failures, m.Checks[0].State, m.Checks[0].LastError = 2, state.Down, "HTTP 502 Bad Gateway"
		} else {
			m.Checks[0].Slow, m.Checks[0].State = 0, state.Up
		}
		m.Projects = []ProjectMinute{{Project: "plop", State: st, Evidence: state.Checked}}
		m.Network = []NetMinute{{Interface: "enp3s0", RxBytes: 1000, TxBytes: 50}, {Interface: "docker0", RxBytes: 1, TxBytes: 2}}
		if err := app.WriteMinute(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	end := base.Add(20 * time.Minute)

	runs, err := reader.StateRuns(ctx, map[string]state.State{"plop": state.Up, "lys": state.Up})
	if err != nil {
		t.Fatal(err)
	}
	if r := runs["plop"]; r.LastOther == nil || !r.LastOther.Equal(base.Add(11*time.Minute)) || r.First == nil || !r.First.Equal(base) {
		t.Errorf("plop's run = %+v, want last other at 14:11, first at 14:00", r)
	}
	if r := runs["lys"]; r.LastOther != nil || r.First != nil {
		t.Errorf("lys, without history: %+v", r)
	}
	if runs, err := reader.StateRuns(ctx, map[string]state.State{"plop": state.Down}); err != nil ||
		!runs["plop"].LastOther.Equal(base.Add(19*time.Minute)) {
		t.Errorf("plop if down now: %+v, %v; want last other at 14:19", runs["plop"], err)
	}

	spans, err := reader.StateSpans(ctx, base, end)
	if err != nil {
		t.Fatal(err)
	}
	want := []Span{
		{Project: "plop", State: state.Up, Start: base, End: base.Add(10 * time.Minute), Minutes: 10},
		{Project: "plop", State: state.Down, Start: base.Add(10 * time.Minute), End: base.Add(12 * time.Minute), Minutes: 2},
		{Project: "plop", State: state.Up, Start: base.Add(12 * time.Minute), End: base.Add(15 * time.Minute), Minutes: 3},
		{Project: "plop", State: state.Up, Start: base.Add(17 * time.Minute), End: end, Minutes: 3},
	}
	if len(spans) != len(want) {
		t.Fatalf("spans = %+v, want %+v", spans, want)
	}
	for i := range want {
		if spans[i] != want[i] {
			t.Errorf("span %d = %+v, want %+v", i, spans[i], want[i])
		}
	}

	troubles, err := reader.ServiceTroubles(ctx, base, end)
	if err != nil || len(troubles) != 2 || troubles[0].Service != "plop" || troubles[0].State != state.Down {
		t.Errorf("service troubles = %+v, %v; want plop down twice", troubles, err)
	}
	checks, err := reader.CheckTroubles(ctx, base, end)
	if err != nil || len(checks) != 2 || checks[1].LastError != "HTTP 502 Bad Gateway" || checks[1].Failures != 2 {
		t.Errorf("check troubles = %+v, %v; want two failed minutes with their error", checks, err)
	}

	usage, err := reader.ServiceUsage(ctx, base, end, 5*time.Minute, "plop")
	if err != nil {
		t.Fatal(err)
	}
	// Two services (plop, migrate), four 5-minute buckets each.
	if len(usage) != 8 || usage[4].Service != "plop" || usage[4].Minutes != 5 || usage[4].NetRx != 30000 ||
		usage[4].CPUAvg == nil || *usage[4].CPUAvg != 0.25 || *usage[4].MemMax != 90<<20 {
		t.Errorf("usage = %+v, %v", usage, err)
	}
	if usage[0].Service != "migrate" || usage[0].CPUAvg != nil || usage[0].MemAvg != nil {
		t.Errorf("a service without readings: %+v", usage[0])
	}
	if other, err := reader.ServiceUsage(ctx, base, end, 5*time.Minute, "lys"); err != nil || len(other) != 0 {
		t.Errorf("another project's usage = %+v, %v", other, err)
	}

	network, err := reader.HostNetwork(ctx, base, end, 10*time.Minute)
	if err != nil || len(network) != 4 || network[2].Interface != "enp3s0" || network[2].Minutes != 10 ||
		network[2].RxBytes != 10000 || network[3].RxBytes != 8000 || network[3].TxBytes != 400 {
		t.Errorf("network = %+v, %v; want docker0 and enp3s0, two buckets each", network, err)
	}

	host, err := reader.HostDetail(ctx, base, end, 10*time.Minute)
	if err != nil || len(host) != 2 {
		t.Fatalf("host = %+v, %v", host, err)
	}
	if h := host[0]; *h.MemUsedAvg != 3<<30 || *h.MemTotal != 16<<30 || *h.SwapUsedAvg != 512<<10 ||
		*h.Load1Max < 0.59 || *h.DiskUsed != 28<<30 || *h.DiskTotal != 250<<30 || h.Samples != 120 {
		t.Errorf("host detail = %+v", h)
	}
}

// TestDatabaseUse reads sparkdb as sonar_monitor, and keeps the readings.
func TestDatabaseUse(t *testing.T) {
	app := open(t)
	ctx := context.Background()
	monitorURL := os.Getenv("SONAR_TEST_DATABASE_MONITOR_URL")
	if monitorURL == "" {
		t.Skip("no SONAR_TEST_DATABASE_MONITOR_URL: run scripts/test-db.sh up again, then eval \"$(scripts/test-db.sh env)\"")
	}
	monitor, err := Connect(ctx, monitorURL, "SONAR_TEST_DATABASE_MONITOR_URL")
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close()

	use, err := monitor.DatabaseUse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if use.MaxConnections < 1 || use.Connections < 1 {
		t.Errorf("server = %+v, want a limit and at least this connection", use)
	}
	var sonar *DatabaseRow
	for i, d := range use.Databases {
		if d.Name == "sonar_db" {
			sonar = &use.Databases[i]
		}
		if d.Name == "template0" || d.Name == "template1" {
			t.Errorf("a template database is listed: %s", d.Name)
		}
	}
	if sonar == nil || sonar.SizeBytes < 1<<20 || sonar.Connections < 1 {
		t.Fatalf("sonar_db = %+v, want its size and connections", sonar)
	}

	// The monitor role reads statistics, never Sonar's tables.
	if _, err := monitor.pool.Exec(ctx, "SELECT * FROM sonar.host_minutes"); err == nil {
		t.Error("sonar_monitor can read sonar's tables")
	}

	base := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	for n := range 3 {
		m := Minute{Start: base.Add(time.Duration(n) * time.Minute), Databases: []DatabaseMinute{
			{Database: "lys_db", Connections: 2 + n, Active: n, SizeBytes: int64(1000 + n)},
		}}
		if err := app.WriteMinute(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	history, err := app.DatabaseHistory(ctx, base, base.Add(time.Hour), time.Hour)
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %+v, %v", history, err)
	}
	if h := history[0]; h.Database != "lys_db" || h.Minutes != 3 || h.ConnAvg != 3 || h.ConnMax != 4 || h.ActiveMax != 2 || h.SizeBytes != 1002 {
		t.Errorf("history = %+v", h)
	}
}

// TestDiskHistory reads back each bucket's last measurement, the totals first.
func TestDiskHistory(t *testing.T) {
	app := open(t)
	ctx := context.Background()
	for _, table := range []string{"project_disk", "disk_totals"} {
		if _, err := app.pool.Exec(ctx, "DELETE FROM sonar."+table); err != nil {
			t.Fatal(err)
		}
	}
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for n := range 3 {
		at := day.Add(time.Duration(10+n) * time.Hour)
		data := int64(1000 * (n + 1))
		m := Minute{Start: at.Truncate(time.Minute), Disk: &DiskMeasurement{MeasuredAt: at, Data: &data, Images: 50, BuildCache: 7,
			Projects: []ProjectDiskRow{{Project: "lys", DataBytes: &data, ImageBytes: 5}, {Project: "plop", ContainerBytes: int64(n)}}}}
		if err := app.WriteMinute(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	points, err := app.DiskHistory(ctx, day, day.AddDate(0, 0, 1), 24*time.Hour)
	if err != nil || len(points) != 3 {
		t.Fatalf("points = %+v, %v", points, err)
	}
	totals, lys, plop := points[0], points[1], points[2]
	if totals.Project != "" || *totals.DataBytes != 3000 || totals.BuildCacheBytes != 7 || !totals.MeasuredAt.Equal(day.Add(12*time.Hour)) {
		t.Errorf("totals = %+v", totals)
	}
	if lys.Project != "lys" || *lys.DataBytes != 3000 || plop.DataBytes != nil || plop.ContainerBytes != 2 {
		t.Errorf("lys = %+v, plop = %+v", lys, plop)
	}
}

// TestTraffic keeps the latest count of each hour and adds hours into buckets.
func TestTraffic(t *testing.T) {
	app := open(t)
	ctx := context.Background()
	hour := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	write := func(hours ...TrafficHour) {
		t.Helper()
		if err := app.WriteMinute(ctx, Minute{Start: time.Now().UTC().Truncate(time.Minute), Traffic: hours}); err != nil {
			t.Fatal(err)
		}
	}
	write(TrafficHour{Host: "plop.mobasity.com", Hour: hour, Requests: 10, Bytes: 100},
		TrafficHour{Host: "plop.mobasity.com", Hour: hour.Add(time.Hour), Requests: 5, Errors: 1, Bytes: 50})
	write(TrafficHour{Host: "plop.mobasity.com", Hour: hour, Requests: 12, Bytes: 120}) // Cloudflare's later count

	got, err := app.TrafficHistory(ctx, hour, hour.Add(24*time.Hour), 24*time.Hour)
	if err != nil || len(got) != 1 || got[0].Requests != 17 || got[0].Errors != 1 || got[0].Bytes != 170 {
		t.Errorf("traffic = %+v, %v; want 17 requests, 1 error, 170 bytes", got, err)
	}
}
