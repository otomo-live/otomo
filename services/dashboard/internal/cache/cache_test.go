package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock so TTL and bucket tests never sleep.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestDoCachesWithinTTL(t *testing.T) {
	clock := newFakeClock()
	c := New(10*time.Second, 64)
	c.SetClock(clock.Now)

	var loads atomic.Int64
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		return []byte("body"), nil
	}

	for i := 0; i < 3; i++ {
		got, err := c.Do(context.Background(), "overview", "k", load)
		if err != nil {
			t.Fatalf("Do #%d: %v", i, err)
		}
		if string(got) != "body" {
			t.Fatalf("Do #%d = %q, want body", i, got)
		}
	}
	if loads.Load() != 1 {
		t.Fatalf("load ran %d times within TTL, want 1", loads.Load())
	}
}

func TestDoExpiresAfterTTL(t *testing.T) {
	clock := newFakeClock()
	c := New(10*time.Second, 64)
	c.SetClock(clock.Now)

	var loads atomic.Int64
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		return []byte(fmt.Sprintf("v%d", loads.Load())), nil
	}

	if _, err := c.Do(context.Background(), "overview", "k", load); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Second)
	got, err := c.Do(context.Background(), "overview", "k", load)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Fatalf("after TTL = %q, want v2", got)
	}
	if loads.Load() != 2 {
		t.Fatalf("load ran %d times, want 2", loads.Load())
	}
}

func TestDoSingleflight(t *testing.T) {
	c := New(10*time.Second, 64)

	var loads atomic.Int64
	release := make(chan struct{})
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		<-release
		return []byte("body"), nil
	}

	const callers = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := c.Do(context.Background(), "overview", "k", load)
			if err != nil {
				errs <- err
				return
			}
			if string(got) != "body" {
				errs <- fmt.Errorf("body = %q", got)
			}
		}()
	}
	close(start)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if loads.Load() != 1 {
		t.Fatalf("concurrent Do ran load %d times, want 1", loads.Load())
	}
}

func TestDoDistinctKeysLoadSeparately(t *testing.T) {
	c := New(10*time.Second, 64)

	var loads atomic.Int64
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		return []byte("body"), nil
	}
	for _, key := range []string{"a", "b", "c"} {
		if _, err := c.Do(context.Background(), "overview", key, load); err != nil {
			t.Fatal(err)
		}
	}
	if loads.Load() != 3 {
		t.Fatalf("load ran %d times, want 3", loads.Load())
	}
}

func TestDoErrorIsNotCached(t *testing.T) {
	c := New(10*time.Second, 64)

	var loads atomic.Int64
	wantErr := errors.New("boom")
	load := func(context.Context) ([]byte, error) {
		if loads.Add(1) == 1 {
			return nil, wantErr
		}
		return []byte("ok"), nil
	}

	if _, err := c.Do(context.Background(), "overview", "k", load); !errors.Is(err, wantErr) {
		t.Fatalf("first Do error = %v, want %v", err, wantErr)
	}
	got, err := c.Do(context.Background(), "overview", "k", load)
	if err != nil {
		t.Fatalf("second Do: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("second Do = %q, want ok", got)
	}
}

func TestBoundEvictsOldest(t *testing.T) {
	c := New(10*time.Second, 64)
	load := func(context.Context) ([]byte, error) { return []byte("x"), nil }

	for i := 0; i < 100; i++ {
		if _, err := c.Do(context.Background(), "overview", fmt.Sprintf("k%d", i), load); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Len(); got != 64 {
		t.Fatalf("Len = %d, want 64", got)
	}

	// The oldest keys are gone and a recent one is present.
	var loads atomic.Int64
	miss := func(context.Context) ([]byte, error) { loads.Add(1); return []byte("y"), nil }
	if _, err := c.Do(context.Background(), "overview", "k0", miss); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 1 {
		t.Error("k0 was not evicted by the cap")
	}
	if _, err := c.Do(context.Background(), "overview", "k99", miss); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 1 {
		t.Error("k99 should still be cached")
	}
}

func TestCallerCancellationDoesNotAbortBatch(t *testing.T) {
	c := New(10*time.Second, 64)

	release := make(chan struct{})
	load := func(context.Context) ([]byte, error) {
		<-release
		return []byte("body"), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Do(ctx, "overview", "k", load)
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled caller did not return")
	}

	close(release)
	// The batch the cancelled caller started still cached its value.
	deadline := time.Now().Add(time.Second)
	for {
		if v, ok := c.get("k"); ok && string(v) == "body" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached batch never cached its value")
		}
		time.Sleep(time.Millisecond)
	}
}

// countingObserver records cache events for assertions.
type countingObserver struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingObserver() *countingObserver {
	return &countingObserver{counts: map[string]int{}}
}

func (o *countingObserver) CacheRequest(endpoint, result string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.counts[endpoint+"/"+result]++
}

func (o *countingObserver) count(endpoint, result string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.counts[endpoint+"/"+result]
}

// TestDoObserverCountsHitMissShared pins the cache seam directly: a first load is a miss,
// the same key again is a hit, and concurrent callers of one in-flight load are shared.
func TestDoObserverCountsHitMissShared(t *testing.T) {
	ctx := context.Background()
	load := func(context.Context) ([]byte, error) { return []byte("body"), nil }

	c := New(10*time.Second, 64)
	obs := newCountingObserver()
	c.SetObserver(obs)
	if _, err := c.Do(ctx, "overview", "k", load); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(ctx, "overview", "k", load); err != nil {
		t.Fatal(err)
	}
	if got := obs.count("overview", "miss"); got != 1 {
		t.Errorf("miss = %d, want 1", got)
	}
	if got := obs.count("overview", "hit"); got != 1 {
		t.Errorf("hit = %d, want 1", got)
	}

	// Ten callers, one in-flight load: exactly one miss and nine shared waits.
	c2 := New(10*time.Second, 64)
	obs2 := newCountingObserver()
	c2.SetObserver(obs2)

	const callers = 10
	var started sync.WaitGroup
	started.Add(callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Done()
			_, _ = c2.Do(ctx, "series", "k", func(context.Context) ([]byte, error) {
				started.Wait()
				time.Sleep(20 * time.Millisecond)
				return []byte("body"), nil
			})
		}()
	}
	wg.Wait()

	if got := obs2.count("series", "miss"); got != 1 {
		t.Errorf("concurrent miss = %d, want 1", got)
	}
	if got := obs2.count("series", "shared"); got != callers-1 {
		t.Errorf("concurrent shared = %d, want %d", got, callers-1)
	}
	if got := obs2.count("series", "hit"); got != 0 {
		t.Errorf("concurrent hit = %d, want 0", got)
	}
}
