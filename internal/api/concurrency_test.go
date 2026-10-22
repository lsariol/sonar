package api

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrentRequests sends many requests at once, across endpoints and
// while the cache expires, for the race detector to look at.
func TestConcurrentRequests(t *testing.T) {
	r := &fakeReader{days: testDays(), response: map[string]int{"plop": 51}, buckets: testBuckets()}
	s := newTestServer(t, r, &fakeSnapshots{snap: testSnapshot(now)})
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	s.opts.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }

	paths := []string{"/v1/status", "/v1/status?days=30", "/v1/status/history", "/v1/status/history?range=7d", "/healthz"}
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%20 == 0 {
				clock.Add(int64(statusCacheFor))
			}
			status, _, _ := do(t, s, "GET", paths[i%len(paths)], "Bearer "+token)
			if status != http.StatusOK {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Errorf("%d of 200 requests failed", n)
	}
	if calls := r.dayCalls.Load(); calls < 2 || calls > 40 {
		t.Errorf("%d database calls for 200 status requests over 10 cache periods; the cache should keep it near 20", calls)
	}
}
