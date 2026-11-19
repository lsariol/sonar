package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lsariol/sonar/internal/cloudflare"
	"github.com/lsariol/sonar/internal/store"
)

const (
	trafficCollector = "cloudflare"

	// trafficEvery is how often the agent reads Cloudflare, each time the last trafficLookback.
	trafficEvery    = 15 * time.Minute
	trafficLookback = 3 * time.Hour

	// trafficBackfill is how far back the first reading goes, so a new Sonar has a week of traffic.
	trafficBackfill = 7 * 24 * time.Hour
	trafficTimeout  = 2 * time.Minute
)

// Traffic is where the agent reads traffic per hostname: Cloudflare.
type Traffic interface {
	Zones(ctx context.Context) ([]cloudflare.Zone, error)
	Traffic(ctx context.Context, zone string, from, to time.Time) ([]cloudflare.Hour, error)
}

// sampleTraffic reads every zone's traffic by hour and files it; the first reading backfills.
func (a *Agent) sampleTraffic(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, trafficTimeout)
	defer cancel()
	now := a.opts.Now().UTC()
	to := now.Truncate(time.Hour).Add(time.Hour)
	from := to.Add(-trafficLookback)
	a.mu.Lock()
	first := a.trafficRead.IsZero()
	a.mu.Unlock()
	if first {
		from = to.Add(-trafficBackfill)
	}

	hours, err := a.readTraffic(ctx, from, to)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.record(trafficCollector, err)
	if len(hours) > 0 {
		b := a.bucketAt(a.opts.Now())
		b.traffic = append(b.traffic, hours...)
	}
	if err == nil {
		a.trafficRead = now
	}
}

// readTraffic reads every zone a day per query; a zone that fails doesn't stop the others.
func (a *Agent) readTraffic(ctx context.Context, from, to time.Time) ([]store.TrafficHour, error) {
	zones, err := a.opts.Traffic.Zones(ctx)
	if err != nil {
		return nil, err
	}
	if len(zones) == 0 {
		return nil, errors.New("Cloudflare: SONAR_CLOUDFLARE_TOKEN can't read any zone: give it Zone Read and Analytics Read on every zone")
	}
	var out []store.TrafficHour
	var errs []error
	for _, z := range zones {
		for start := from; start.Before(to); start = start.Add(24 * time.Hour) {
			end := start.Add(24 * time.Hour)
			if end.After(to) {
				end = to
			}
			hours, err := a.opts.Traffic.Traffic(ctx, z.ID, start, end)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", z.Name, err))
				break
			}
			for _, h := range hours {
				out = append(out, store.TrafficHour{Host: h.Host, Hour: h.Hour, Requests: h.Requests, Errors: h.Errors, Bytes: h.Bytes})
			}
		}
	}
	return out, errors.Join(errs...)
}
