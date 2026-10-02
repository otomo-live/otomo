// Package cache is the Dashboard's bounded response cache: pre-serialised JSON keyed by
// endpoint, canonical parameters and a time bucket, with a TTL and a hard entry cap.
//
// Concurrent misses for the same key are collapsed by a singleflight group, so N clients
// polling the same overview at the same instant run one upstream batch and all read the
// same bytes. The cache never encodes JSON itself; it stores and returns []byte, which is
// what keeps a hit free of any marshalling work.
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Defaults for a response cache. TTL is the documented overview staleness bound; MaxEntries
// keeps a long-lived process from growing without bound as time buckets roll.
const (
	DefaultTTL        = 10 * time.Second
	DefaultMaxEntries = 64
)

// Cache is a bounded TTL cache safe for concurrent use. The zero value is not usable; build
// one with New.
type Cache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	now   func() time.Time
	ll    *list.List
	items map[string]*list.Element
	group singleflight.Group
	obs   Observer
}

// Observer receives one event per Do call so the service can expose a cache hit ratio
// without this package importing a metrics library. An endpoint is a caller-supplied,
// closed label ("overview", "series") and result is hit, miss or shared, where shared means
// this caller waited on another caller's in-flight load rather than running its own.
type Observer interface {
	CacheRequest(endpoint, result string)
}

// SetObserver wires optional instrumentation. Call it before the cache serves a request.
// A nil observer disables instrumentation.
func (c *Cache) SetObserver(o Observer) {
	c.mu.Lock()
	c.obs = o
	c.mu.Unlock()
}

// observe reports one cache event, if instrumentation is wired.
func (c *Cache) observe(endpoint, result string) {
	c.mu.Lock()
	o := c.obs
	c.mu.Unlock()
	if o != nil {
		o.CacheRequest(endpoint, result)
	}
}

// entry is one cached response. expires is an absolute time so a clock change cannot make an
// old entry live forever.
type entry struct {
	key     string
	value   []byte
	expires time.Time
}

// New returns a cache holding at most max entries for ttl each. A non-positive ttl or max is
// replaced by the default, so a half-wired caller is bounded rather than unbounded.
func New(ttl time.Duration, max int) *Cache {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if max <= 0 {
		max = DefaultMaxEntries
	}
	return &Cache{
		ttl:   ttl,
		max:   max,
		now:   time.Now,
		ll:    list.New(),
		items: make(map[string]*list.Element),
	}
}

// SetClock replaces the cache's clock. It is for tests that need to advance time without
// sleeping; call it before the cache serves a request.
func (c *Cache) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// Do returns the cached value for key, or calls load once and caches its result. Every
// caller that misses while a load for the same key is in flight waits for that load rather
// than starting another.
//
// endpoint is a closed label used only for metrics ("overview", "series"); it does not take
// part in the key. The load runs with a context detached from the initiating caller's
// cancellation, so one disconnected client cannot abort a batch other clients are waiting
// on; ctx still bounds how long a calling goroutine waits for the result.
func (c *Cache) Do(ctx context.Context, endpoint, key string, load func(context.Context) ([]byte, error)) ([]byte, error) {
	if v, ok := c.get(key); ok {
		c.observe(endpoint, "hit")
		return v, nil
	}

	// ran is written only by the goroutine that executes load (the singleflight leader)
	// and read after receiving from ch, so the channel is the happens-before edge.
	ran := false
	ch := c.group.DoChan(key, func() (any, error) {
		ran = true
		// Another flight may have filled the key between the miss and this call.
		if v, ok := c.get(key); ok {
			return v, nil
		}

		batchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()

		v, err := load(batchCtx)
		if err != nil {
			return nil, err
		}
		c.set(key, v)
		return v, nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if ran {
			c.observe(endpoint, "miss")
		} else {
			c.observe(endpoint, "shared")
		}
		if res.Err != nil {
			return nil, res.Err
		}
		v, _ := res.Val.([]byte)
		return v, nil
	}
}

// Len returns the number of live entries. Expired entries are not counted.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked()
	return len(c.items)
}

// get returns a live value, evicting it first when its TTL has passed.
func (c *Cache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if !c.now().Before(e.expires) {
		c.removeLocked(el)
		return nil, false
	}
	return e.value, true
}

// set inserts or refreshes an entry, then trims the oldest until the cache is within its
// cap. Refreshing an existing key keeps its place in the eviction order.
func (c *Cache) set(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry)
		e.value = value
		e.expires = now.Add(c.ttl)
		return
	}

	el := c.ll.PushBack(&entry{key: key, value: value, expires: now.Add(c.ttl)})
	c.items[key] = el

	for c.ll.Len() > c.max {
		c.removeLocked(c.ll.Front())
	}
}

// expireLocked drops every entry whose TTL has passed.
func (c *Cache) expireLocked() {
	now := c.now()
	for el := c.ll.Front(); el != nil; {
		next := el.Next()
		if !now.Before(el.Value.(*entry).expires) {
			c.removeLocked(el)
		}
		el = next
	}
}

// removeLocked removes one list element from both the list and the index.
func (c *Cache) removeLocked(el *list.Element) {
	if el == nil {
		return
	}
	c.ll.Remove(el)
	delete(c.items, el.Value.(*entry).key)
}
