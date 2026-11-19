package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/state"
	"github.com/lsariol/sonar/internal/store"
)

func TestDatabasesCommand(t *testing.T) {
	snap := testSnap(now)
	if _, _, err := run(t, testCLI(t, &snap, nil), "databases"); err == nil || !strings.Contains(err.Error(), "hasn't been read yet") {
		t.Errorf("before sparkdb is read: %v", err)
	}
	oldest := int64(90)
	snap.Databases = &snapshot.DatabaseServer{ReadAt: now, MaxConnections: 100, Connections: 7, Databases: []snapshot.Database{
		{Name: "AdminDB", SizeBytes: 8 << 20},
		{Name: "lys_db", Project: "lys", SizeBytes: 300 << 20, Connections: 5, Active: 1, OldestXactSecs: &oldest},
	}}
	o, e, err := run(t, testCLI(t, &snap, nil), "db")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "databases", o, "DATABASE", "lys_db", "lys", "300 MiB", "1 min", "AdminDB")
	if strings.Index(o, "lys_db") > strings.Index(o, "AdminDB") {
		t.Errorf("not largest first:\n%s", o)
	}
	mustContain(t, "databases", e, "2 databases; sparkdb has 7 of its 100 connections in use")
}

func TestDiskCommand(t *testing.T) {
	snap := testSnap(now)
	if _, _, err := run(t, testCLI(t, &snap, nil), "disk"); err == nil || !strings.Contains(err.Error(), "every 30 minutes") {
		t.Errorf("before a measurement: %v", err)
	}
	data, total := int64(5<<30), int64(5<<30)
	snap.Storage = &snapshot.DiskUse{MeasuredAt: now, Seconds: 12.4,
		Totals: snapshot.DiskTotals{Data: &total, Images: 2 << 30, BuildCache: 1 << 30},
		Projects: []snapshot.ProjectDisk{
			{Project: "plop", ImageBytes: 100 << 20},
			{Project: "sparkdb", DataBytes: &data, DataUnreadable: 3, ImageBytes: 400 << 20},
		}}
	o, e, err := run(t, testCLI(t, &snap, nil), "disk")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "disk", o, "DATA FOLDER", "sparkdb", "5.0 GiB (short)", "plop")
	if strings.Index(o, "sparkdb") > strings.Index(o, "plop") {
		t.Errorf("not largest first:\n%s", o)
	}
	mustContain(t, "disk", e, "2 projects", "build cache 1.0 GiB", "in 12 s", `"sparkdb" (3 entries)`)
}

func TestTrafficCommand(t *testing.T) {
	snap := testSnap(now)
	db := goodDB()
	o, e, err := run(t, testCLI(t, &snap, db), "traffic")
	if err != nil || o != "" || !strings.Contains(e, "No traffic from Cloudflare in the last 7 days") {
		t.Errorf("no traffic: %q %q %v", o, e, err)
	}
	db.traffic = []store.TrafficBucket{
		{Host: "dibs.mobasity.com", Requests: 40, Bytes: 1000},
		{Host: "plop.mobasity.com", Requests: 900, Errors: 2, Bytes: 5 << 20},
		{Host: "plop.mobasity.com", Requests: 300, Errors: 1, Bytes: 1 << 20},
	}
	o, e, err = run(t, testCLI(t, &snap, db), "traffic --days 30")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "traffic", o, "SITE", "plop.mobasity.com", "plop", "1200", "6.0 MiB", "dibs.mobasity.com")
	if strings.Index(o, "plop.mobasity.com") > strings.Index(o, "dibs.mobasity.com") {
		t.Errorf("not busiest first:\n%s", o)
	}
	mustContain(t, "traffic", e, "2 sites", "last 30 days")
	if _, _, err := run(t, testCLI(t, &snap, db), "traffic --days 0"); err == nil || !strings.Contains(err.Error(), "Usage: traffic") {
		t.Errorf("--days 0: %v", err)
	}
}

func TestAppsCommand(t *testing.T) {
	snap := testSnap(now)
	db := goodDB()
	_, e, err := run(t, testCLI(t, &snap, db), "apps")
	if err != nil || !strings.Contains(e, "com.mobasity.report-port") {
		t.Errorf("no reports: %q %v", e, err)
	}
	read := now.Add(-20 * time.Second)
	snap.Apps = []snapshot.AppReport{
		{Project: "lys", Service: "lys", Container: "lys", ReadAt: &read,
			Checks: []snapshot.AppCheck{{Name: "twitch_chat", OK: false, Message: "disconnected: EOF", Since: now.Add(-time.Hour)}}},
		{Project: "maroo", Service: "maroo", Container: "maroo", Error: "GET http://maroo:8080/report/v1: connection refused"},
	}
	today := midnightUTC(now)
	db.appDays = []store.AppCounterDay{
		{Project: "lys", Name: "commands_run", Day: today.AddDate(0, 0, -1), Total: 40},
		{Project: "lys", Name: "commands_run", Day: today, Total: 12},
	}
	o, e, err := run(t, testCLI(t, &snap, db), "apps")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "apps", o, "COUNTER", "commands_run", "12", "52", "twitch_chat", "failing", "disconnected: EOF")
	mustContain(t, "apps", e, "2 apps reporting, 1 counter, 1 check", "Couldn't read the report of maroo/maroo", "Failing: lys/twitch_chat")

	o, _, _ = run(t, testCLI(t, &snap, db), "apps maroo")
	if strings.Contains(o, "commands_run") {
		t.Errorf("apps maroo shows lys's counter:\n%s", o)
	}
}

func TestStatesCommand(t *testing.T) {
	snap := testSnap(now)
	db := goodDB()
	at := now.Truncate(time.Minute).Add(-3 * time.Hour)
	m := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	db.spans = []store.Span{
		{Project: "plop", State: state.Up, Start: m(0), End: m(60), Minutes: 60},
		{Project: "plop", State: state.Down, Start: m(60), End: m(65), Minutes: 5},
		{Project: "plop", State: state.Up, Start: m(65), End: m(180), Minutes: 115},
	}
	db.serviceTrouble = []store.ServiceTrouble{{Project: "plop", Service: "plop", Minute: m(61), State: state.Down}}
	db.checkTrouble = []store.CheckTrouble{{Project: "plop", Service: "plop", URL: "https://plop.mobasity.com", Minute: m(61),
		State: state.Down, Failures: 2, LastError: "HTTP 502 Bad Gateway"}}
	o, e, err := run(t, testCLI(t, &snap, db), "states plop")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "states", o, "LASTED", "down", "5 min", "plop down 1 min", "https://plop.mobasity.com down, 2 failed: HTTP 502 Bad Gateway")
	mustContain(t, "states", e, `3 spans of "plop" over the last 7 days, 1 not operational`)

	if _, _, err := run(t, testCLI(t, &snap, db), "states nope"); err == nil || !strings.Contains(err.Error(), `No project "nope"`) {
		t.Errorf("an unknown project: %v", err)
	}
	if _, _, err := run(t, testCLI(t, &snap, db), "states plop --days 31"); err == nil || !strings.Contains(err.Error(), "from 1 to 30") {
		t.Errorf("--days 31: %v", err)
	}
}
