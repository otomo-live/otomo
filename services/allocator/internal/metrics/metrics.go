// Package metrics owns the Allocator's domain instruments: the size of the game-server
// pool by state, allocation outcomes and handling time, what the reaper ended, and how
// the Session callback drain fared.
//
// The registry itself belongs to internal/server; this package registers on it through
// Server.Registry, so there is still one registry per server instance and the HTTP
// instruments (allocator_http_*) and these domain instruments share one /metrics.
//
// Every method on *Metrics is safe on a nil receiver and records nothing, so a caller
// without a registry — most handler tests — passes nil rather than building a throwaway
// one.
package metrics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// serverStates mirrors the game_server.state CHECK exactly. The collector emits all of
// them on every scrape, 0 included, so a graph shows a real zero for an empty state
// rather than a missing series.
var serverStates = []string{"free", "reserved", "busy", "dead"}

// scrapeTimeout bounds the game-server count's query. The metrics listener is
// unauthenticated and separate from the API, so a slow or wedged Postgres must not hold
// a scrape open; a timed-out scrape emits no server series and is counted as an error.
const scrapeTimeout = 2 * time.Second

// The result label values for allocator_allocations_total.
const (
	ResultCreated    = "created"
	ResultExisting   = "existing"
	ResultNoCapacity = "no_capacity"
	ResultConflict   = "conflict"
	ResultInvalid    = "invalid"
	ResultError      = "error"
)

// The kind label values for allocator_reaped_total.
const (
	KindDead    = "dead"
	KindExpired = "expired"
)

// The result label values for allocator_callbacks_total.
const (
	CallbackDelivered = "delivered"
	CallbackRetry     = "retry"
	CallbackGaveUp    = "gave_up"
)

// Metrics holds the Allocator's domain collectors. Build it once with New; the
// collectors are safe for concurrent use, and a nil *Metrics is a no-op.
type Metrics struct {
	servers      *serverCollector
	scrapeErrors prometheus.Counter

	allocations       *prometheus.CounterVec
	allocationSeconds prometheus.Histogram

	reaped *prometheus.CounterVec

	callbacks *prometheus.CounterVec
}

// New builds the collectors, registers them on reg (skipping registration when reg is
// nil), and pre-initialises every counter label combination to 0.
//
// db backs the allocator_servers gauge; it is read on each scrape, not on a timer, so
// the gauge is always as fresh as the scrape. A nil db is allowed and simply emits no
// server series, which is what lets a test build a Metrics without a database.
func New(reg prometheus.Registerer, db *pgxpool.Pool) *Metrics {
	m := &Metrics{
		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "allocator_servers_scrape_errors_total",
			Help: "Scrapes of the game-server state gauge that failed to query Postgres.",
		}),
		allocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "allocator_allocations_total",
			Help: "Allocations requested, by result.",
		}, []string{"result"}),
		allocationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "allocator_allocation_seconds",
			Help:    "POST /internal/allocations handling time in seconds.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}),
		reaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "allocator_reaped_total",
			Help: "Allocations ended by the reaper, by kind.",
		}, []string{"kind"}),
		callbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "allocator_callbacks_total",
			Help: "Session callbacks drained, by result.",
		}, []string{"result"}),
	}
	m.servers = newServerCollector(db, m.scrapeErrors)

	if reg != nil {
		reg.MustRegister(
			m.scrapeErrors,
			m.servers,
			m.allocations,
			m.allocationSeconds,
			m.reaped,
			m.callbacks,
		)
	}
	m.initLabels()
	return m
}

// initLabels touches every series once so it exists at 0 before the first event. A
// dashboard can then show "no allocations yet" instead of a gap, and a rate() over a
// counter that has just come up does not start at an absent series.
func (m *Metrics) initLabels() {
	for _, result := range []string{
		ResultCreated,
		ResultExisting,
		ResultNoCapacity,
		ResultConflict,
		ResultInvalid,
		ResultError,
	} {
		m.allocations.WithLabelValues(result).Add(0)
	}
	for _, kind := range []string{KindDead, KindExpired} {
		m.reaped.WithLabelValues(kind).Add(0)
	}
	for _, result := range []string{CallbackDelivered, CallbackRetry, CallbackGaveUp} {
		m.callbacks.WithLabelValues(result).Add(0)
	}
}

// RecordAllocation counts one POST /internal/allocations attempt under result and
// observes how long the handler took. A nil receiver records nothing.
func (m *Metrics) RecordAllocation(result string, d time.Duration) {
	if m == nil {
		return
	}
	m.allocations.WithLabelValues(result).Inc()
	m.allocationSeconds.Observe(d.Seconds())
}

// RecordReaped adds n to the counter for kind. The reaper calls it only after a
// successful sweep, so an error is not counted as work.
func (m *Metrics) RecordReaped(kind string, n int) {
	if m == nil {
		return
	}
	m.reaped.WithLabelValues(kind).Add(float64(n))
}

// RecordCallback counts one ended-callback outcome. Each failure is exactly one of
// retry or gave_up, never both.
func (m *Metrics) RecordCallback(result string) {
	if m == nil {
		return
	}
	m.callbacks.WithLabelValues(result).Inc()
}

// AllocationCounter returns the single counter for one result. It exists so a test can
// assert one series with testutil.ToFloat64 instead of collecting the whole vector,
// which has every result label pre-initialised.
func (m *Metrics) AllocationCounter(result string) prometheus.Counter {
	return m.allocations.WithLabelValues(result)
}

// serverCollector is the allocator_servers gauge. It is a custom Collector rather than
// a GaugeVec because its values come from one grouped query at scrape time: the
// database is the source of truth and nothing has to be pushed into a Go-side map that
// could drift.
type serverCollector struct {
	db     *pgxpool.Pool
	desc   *prometheus.Desc
	errors prometheus.Counter
}

func newServerCollector(db *pgxpool.Pool, errors prometheus.Counter) *serverCollector {
	return &serverCollector{
		db:     db,
		errors: errors,
		desc: prometheus.NewDesc(
			"allocator_servers",
			"Game servers in the pool, by state.",
			[]string{"state"},
			nil,
		),
	}
}

// Describe reports one desc for all four states: the label value is chosen at Collect
// time, so the collector is a checked collector and a duplicate registration is caught
// at startup.
func (c *serverCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect runs the grouped count and emits every state. On any query error it emits
// nothing for this scrape and bumps allocator_servers_scrape_errors_total, so a
// database outage shows as a scrape error rather than as an empty pool.
func (c *serverCollector) Collect(ch chan<- prometheus.Metric) {
	if c.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	counts, err := c.count(ctx)
	if err != nil {
		c.errors.Inc()
		return
	}
	for _, state := range serverStates {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, counts[state], state)
	}
}

// count returns one value per known state, defaulting a state Postgres did not return
// to 0.
func (c *serverCollector) count(ctx context.Context) (map[string]float64, error) {
	rows, err := c.db.Query(ctx, `SELECT state, count(*) FROM game_server GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]float64, len(serverStates))
	for _, state := range serverStates {
		counts[state] = 0
	}
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		counts[state] = float64(n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}
