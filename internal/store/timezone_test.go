package store

import (
	"context"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/state"
)

// TestSessionTimeZone: daily rows stay on their UTC day even when a database
// session runs in another time zone (pgx sends dates as calendar dates).
func TestSessionTimeZone(t *testing.T) {
	open(t)
	ctx := context.Background()
	app, err := Connect(ctx, appURL+"&timezone=America/Toronto", "app")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	reader, err := Connect(ctx, readerURL+"&timezone=America/Toronto", "reader")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	m := sampleMinute(day.Add(10 * time.Hour))
	m.Projects[0].State = state.Up
	if err := app.WriteMinute(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := app.Rollup(ctx, day); err != nil {
		t.Fatal(err)
	}
	rows, err := reader.ProjectDays(ctx, day, day.Add(15*time.Hour))
	if err != nil || len(rows) != 1 || !rows[0].Day.Equal(day) || rows[0].Up != 1 {
		t.Errorf("ProjectDays in Toronto time = %+v, %v; want one row for 2026-10-21", rows, err)
	}
}
