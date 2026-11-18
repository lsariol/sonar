package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/store"
)

type fakeMonitor struct {
	use store.DatabaseUse
	err error
}

func (f *fakeMonitor) DatabaseUse(context.Context) (store.DatabaseUse, error) { return f.use, f.err }

func TestDatabaseProject(t *testing.T) {
	cases := map[string]string{"lys_db": "lys", "mobasity_web_db": "mobasity-web", "sonar_db": "sonar", "AdminDB": "", "postgres": ""}
	for name, want := range cases {
		if got := databaseProject(name); got != want {
			t.Errorf("databaseProject(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestDatabases: each database is matched to its running project, goes into
// the minute, and a failed connection is retried at the next reading.
func TestDatabases(t *testing.T) {
	d := &fakeDocker{list: []containers.Container{running('a', "lys", "lys", ""), running('b', "mobasity-web", "web", "")}}
	a, clk := newTestAgent(t, d, &fakeChecker{})
	m := &fakeMonitor{use: store.DatabaseUse{MaxConnections: 100, Connections: 90, Databases: []store.DatabaseRow{
		{Name: "AdminDB", SizeBytes: 1},
		{Name: "lys_db", SizeBytes: 2000, Connections: 4, Active: 1},
		{Name: "mobasity_web_db", SizeBytes: 3000, Connections: 2},
		{Name: "gone_db", SizeBytes: 4000}, // a project that no longer runs
	}}}
	opens := 0
	a.opts.OpenMonitor = func(context.Context) (DatabaseMonitor, error) {
		opens++
		if opens == 1 {
			return nil, errors.New("database: can't reach the database at SONAR_DATABASE_MONITOR_URL: refused")
		}
		return m, nil
	}
	a.collectors[databasesCollector] = &collector{every: databasesEvery}
	a.sampleDocker(context.Background())

	a.sampleDatabases(context.Background())
	if st := a.build().Collectors[databasesCollector]; st.State != snapshot.Unavailable || a.build().Databases != nil {
		t.Fatalf("after a failed connection: %+v", st)
	}
	a.sampleDatabases(context.Background())
	a.sampleDatabases(context.Background())
	if opens != 2 {
		t.Errorf("opened %d times, want 2 (once failed, then kept)", opens)
	}

	s := a.build()
	if s.Databases == nil || s.Databases.Connections != 90 || len(s.Databases.Databases) != 4 {
		t.Fatalf("databases = %+v", s.Databases)
	}
	projects := map[string]string{}
	for _, db := range s.Databases.Databases {
		projects[db.Name] = db.Project
	}
	if projects["lys_db"] != "lys" || projects["mobasity_web_db"] != "mobasity-web" || projects["gone_db"] != "" || projects["AdminDB"] != "" {
		t.Errorf("projects = %v", projects)
	}
	if len(s.Conditions) != 1 || s.Conditions[0].ID != "connections" || s.Conditions[0].Level != "warning" {
		t.Errorf("conditions = %+v, want sparkdb's connections at 90%%", s.Conditions)
	}
	minute := a.bucketAt(clk.t).minute()
	if len(minute.Databases) != 4 || minute.Databases[1].Database != "lys_db" || minute.Databases[1].Connections != 4 {
		t.Errorf("minute databases = %+v", minute.Databases)
	}
}
