package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/cloudflare"
	"github.com/lsariol/sonar/internal/snapshot"
)

type fakeTraffic struct {
	zones   []cloudflare.Zone
	fail    map[string]bool
	queries []string
}

func (f *fakeTraffic) Zones(context.Context) ([]cloudflare.Zone, error) { return f.zones, nil }

func (f *fakeTraffic) Traffic(_ context.Context, zone string, from, to time.Time) ([]cloudflare.Hour, error) {
	f.queries = append(f.queries, zone+" "+from.Format("01-02T15")+" "+to.Format("01-02T15"))
	if f.fail[zone] {
		return nil, errors.New("Cloudflare: reading traffic: no access")
	}
	return []cloudflare.Hour{{Host: zone + ".example", Hour: from, Requests: 1}}, nil
}

// TestTrafficReadings: the first reading backfills a week, later ones three hours; a failing zone stops nothing.
func TestTrafficReadings(t *testing.T) {
	f := &fakeTraffic{zones: []cloudflare.Zone{{ID: "z1", Name: "mobasity.com"}, {ID: "z2", Name: "lucassariol.com"}},
		fail: map[string]bool{"z2": true}}
	a, clk := newTestAgent(t, &fakeDocker{}, &fakeChecker{})
	a.opts.Traffic = f
	a.collectors[trafficCollector] = &collector{every: trafficEvery}

	a.sampleTraffic(context.Background())
	if len(f.queries) != 8 { // seven days for z1, then z2's first day fails
		t.Fatalf("queries = %v", f.queries)
	}
	st := a.build().Collectors[trafficCollector]
	if st.State != snapshot.Unavailable || !strings.Contains(st.Error, "lucassariol.com: Cloudflare") {
		t.Errorf("collector = %+v", st)
	}
	if m := a.bucketAt(clk.t).minute(); len(m.Traffic) != 7 {
		t.Errorf("filed %d hours, want z1's 7", len(m.Traffic))
	}

	f.fail, f.queries = nil, nil
	a.sampleTraffic(context.Background()) // still the backfill: the last one failed
	a.sampleTraffic(context.Background())
	if n := len(f.queries); n != 16 {
		t.Fatalf("after a good backfill: %d queries, want 14 then 2: %v", n, f.queries)
	}
	if last := f.queries[15]; last != "z2 10-21T13 10-21T16" {
		t.Errorf("the next reading = %q, want the last three hours", last)
	}

	f.zones = nil
	a.sampleTraffic(context.Background())
	if st := a.build().Collectors[trafficCollector]; !strings.Contains(st.Error, "can't read any zone") {
		t.Errorf("no zones: %+v", st)
	}
}
