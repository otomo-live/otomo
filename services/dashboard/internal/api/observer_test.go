package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/health"
	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
)

// eventObserver records the api.Observer seam for assertions. It is safe for concurrent use
// because the overview batch emits one degraded event per query goroutine, and concurrent
// cache lookups all report through it.
type eventObserver struct {
	mu       sync.Mutex
	cache    map[string]int
	degraded map[string]int
	tails    int
}

func newEventObserver() *eventObserver {
	return &eventObserver{cache: map[string]int{}, degraded: map[string]int{}}
}

func (o *eventObserver) CacheRequest(endpoint, result string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cache[endpoint+"/"+result]++
}

func (o *eventObserver) OverviewDegraded(card string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.degraded[card]++
}

func (o *eventObserver) TailStreams(delta int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tails += delta
}

func (o *eventObserver) cacheCount(endpoint, result string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cache[endpoint+"/"+result]
}

func (o *eventObserver) degradedCount(card string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.degraded[card]
}

// TestOverviewDegradedObserver pins the degraded seam: a failing query reports exactly its
// own card, with the identifier the response's degraded list carries, and leaves the other
// cards uncounted.
func TestOverviewDegradedObserver(t *testing.T) {
	behavior := fullBehavior(t, "gateway")
	behavior[queryFor(t, "rps")] = queryBehavior{err: errors.New("boom")}

	obs := newEventObserver()
	h := &Handlers{
		Metrics:   &fakeMetrics{behavior: behavior},
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
		Observer:  obs,
	}

	if code := callOverview(h).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := obs.degradedCount("services.rps"); got != 1 {
		t.Errorf("degraded(services.rps) = %d, want 1", got)
	}
	if got := obs.degradedCount("services.p95_ms"); got != 0 {
		t.Errorf("degraded(services.p95_ms) = %d, want 0", got)
	}
}

// TestOverviewCacheMetricsHitMissShared is DSH-C12 from the cache's side: ten concurrent
// overview requests collapse onto one load, reported as exactly one miss and nine shared
// singleflight waits, and the next request inside the same bucket is a hit.
func TestOverviewCacheMetricsHitMissShared(t *testing.T) {
	behavior := fullBehavior(t, "gateway")
	for q, b := range behavior {
		b.delay = 100 * time.Millisecond
		behavior[q] = b
	}

	obs := newEventObserver()
	h := &Handlers{
		Metrics:   &fakeMetrics{behavior: behavior},
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
		Now:       newFakeClock().Now,
		Observer:  obs,
	}

	const callers = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			callOverview(h)
		}()
	}
	close(start)
	wg.Wait()

	if got := obs.cacheCount("overview", "miss"); got != 1 {
		t.Errorf("cache miss = %d, want 1", got)
	}
	if got := obs.cacheCount("overview", "shared"); got != callers-1 {
		t.Errorf("cache shared = %d, want %d", got, callers-1)
	}
	if got := obs.cacheCount("overview", "hit"); got != 0 {
		t.Errorf("cache hit = %d, want 0 while the flight was in progress", got)
	}

	// The batch is cached for the same time bucket: the next request is a hit.
	callOverview(h)
	if got := obs.cacheCount("overview", "hit"); got != 1 {
		t.Errorf("cache hit after the batch = %d, want 1", got)
	}
}
