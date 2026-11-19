package store

import (
	"context"
	"testing"
	"time"
)

// TestApps writes counters and check changes, rolls the day up, and reads
// both back as the API does.
func TestApps(t *testing.T) {
	app := open(t)
	ctx := context.Background()
	reader, err := Connect(ctx, readerURL, "SONAR_TEST_DATABASE_READER_URL")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	for n := range 3 {
		m := Minute{Start: base.Add(time.Duration(n) * time.Minute),
			AppCounters: []AppCounter{{Project: "dibs", Name: "games_completed", Increase: uint64(n + 1)}}}
		if n == 1 {
			m.AppChecks = []AppCheckChange{{Project: "lys", Service: "lys", Name: "twitch_chat", At: m.Start.Add(5 * time.Second), OK: false, Message: "EOF"}}
		}
		if err := app.WriteMinute(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.WriteMinute(ctx, Minute{Start: base, AppCounters: []AppCounter{{Project: "dibs", Name: "games_completed", Increase: 1}}}); err != nil {
		t.Fatal(err) // a retried minute replaces, never adds
	}
	if err := app.Rollup(ctx, base); err != nil {
		t.Fatal(err)
	}

	days, err := reader.AppCounterDays(ctx, base, base)
	if err != nil || len(days) != 1 || days[0].Total != 6 || !days[0].Day.Equal(midnightUTC(base)) {
		t.Errorf("days = %+v, %v; want 6 games on the day", days, err)
	}
	changes, err := reader.AppCheckChanges(ctx, base)
	if err != nil || len(changes) != 1 || changes[0].OK || changes[0].Message != "EOF" {
		t.Errorf("changes = %+v, %v", changes, err)
	}
}
