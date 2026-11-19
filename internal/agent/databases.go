package agent

import (
	"context"
	"strings"
	"time"

	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/store"
)

const (
	databasesCollector = "databases"
	databasesEvery     = time.Minute
	databasesTimeout   = 10 * time.Second
)

// DatabaseMonitor reads every database on sparkdb: sonar_monitor's view.
type DatabaseMonitor interface {
	DatabaseUse(ctx context.Context) (store.DatabaseUse, error)
}

// sampleDatabases reads sparkdb's databases, connecting first; a failed connection is retried next time.
func (a *Agent) sampleDatabases(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, databasesTimeout)
	defer cancel()
	var use store.DatabaseUse
	err := a.connectMonitor(ctx)
	if err == nil {
		use, err = a.monitor.DatabaseUse(ctx)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.record(databasesCollector, err)
	if err != nil {
		return
	}
	now := a.opts.Now()
	a.databases, a.databasesAt = &use, now
	b := a.bucketAt(now)
	b.databases = use.Databases
}

// connectMonitor opens the monitor connection once.
func (a *Agent) connectMonitor(ctx context.Context) error {
	if a.monitor != nil {
		return nil
	}
	m, err := a.opts.OpenMonitor(ctx)
	if err != nil {
		return err
	}
	a.monitor = m
	return nil
}

// buildDatabases is the last reading of sparkdb, each database matched to its project; callers hold a.mu.
func (a *Agent) buildDatabases() *snapshot.DatabaseServer {
	if a.databases == nil {
		return nil
	}
	projects := make(map[string]bool)
	for _, c := range a.containers {
		projects[c.Project] = c.Project != ""
	}
	d := &snapshot.DatabaseServer{ReadAt: a.databasesAt.UTC(), MaxConnections: a.databases.MaxConnections,
		Connections: a.databases.Connections, Databases: []snapshot.Database{}}
	for _, row := range a.databases.Databases {
		db := snapshot.Database{Name: row.Name, SizeBytes: row.SizeBytes, Connections: row.Connections, Active: row.Active,
			OldestXactSecs: row.OldestXactSecs}
		if p := databaseProject(row.Name); projects[p] {
			db.Project = p
		}
		d.Databases = append(d.Databases, db)
	}
	return d
}

// databaseProject is the project a database name points to: mobasity_web_db is mobasity-web.
func databaseProject(name string) string {
	base, ok := strings.CutSuffix(name, "_db")
	if !ok {
		return ""
	}
	return strings.ReplaceAll(base, "_", "-")
}
