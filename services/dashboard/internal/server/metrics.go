package server

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
)

// metrics holds the service's Prometheus instruments and the registry they are registered
// on.
//
// Labels follow COM-10: route is the mux pattern with the method stripped, method and
// status are their own labels, and nothing here carries a service name, a staff identifier
// or a raw URL. `reason` on the rejection counter is the one label outside that document's
// list, and it is there because 05-gateway-techspec.md §8 defines the same series for
// Gateway: an operator comparing the two services' rejection counts should be reading the
// same vocabulary on both.
//
// DSH-C12 adds upstream query latency and cache hit ratio here; nothing in this table is
// shipped to Prometheus by anyone but this process, so the metrics endpoint is the whole
// story for the Dashboard's own health.
type metrics struct {
	registry      *prometheus.Registry
	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	tokenRejected *prometheus.CounterVec
	buildInfo     *prometheus.GaugeVec

	// Service-prober instruments. service labels are target names from
	// DASHBOARD_TARGETS, so their cardinality is bounded by configuration, not by
	// traffic.
	serviceUp     *prometheus.GaugeVec
	serviceReady  *prometheus.GaugeVec
	probeDuration *prometheus.HistogramVec

	// DSH-C12 upstream and cache instruments. Every label here comes from a closed set
	// defined in this package or its callers; no service name, URL or client string ever
	// reaches them.
	upstreamRequests *prometheus.CounterVec
	upstreamDuration *prometheus.HistogramVec
	cacheRequests    *prometheus.CounterVec
	tailStreams      prometheus.Gauge
	overviewDegraded *prometheus.CounterVec
}

// The closed label sets behind dashboard_upstream_requests_total,
// dashboard_cache_requests_total and dashboard_overview_degraded_total. Declaring them
// here, and pre-initialising them in newMetrics, is what keeps the series set bounded and
// present from the first scrape rather than appearing when a label value is first seen.
var (
	upstreamNames   = []string{"prometheus", "loki", "config_audit", "admin_auth_audit", "session_audit"}
	upstreamResults = []string{"ok", "error", "timeout"}
	cacheEndpoints  = []string{"overview", "series"}
	cacheResults    = []string{"hit", "miss", "shared"}
)

// newMetrics builds a fresh registry rather than registering on the global default one, so
// two servers in a single test binary do not collide and neither inherits whatever else has
// been registered process-wide. version is published as dashboard_build_info; it is set
// once here and never updated, so a new build needs a restart to be reflected — which a
// deploy is.
func newMetrics(version string) *metrics {
	reg := prometheus.NewRegistry()

	m := &metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dashboard_http_requests_total",
			Help: "HTTP requests handled, by route, method and status.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "dashboard_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by route and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		tokenRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dashboard_token_rejected_total",
			Help: "Staff tokens refused by this service, by reason.",
		}, []string{"reason"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dashboard_build_info",
			Help: "Build information; the sample value is always 1.",
		}, []string{"version"}),
		serviceUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dashboard_service_up",
			Help: "Whether the service answered /readyz at all (1) or not (0).",
		}, []string{"service"}),
		serviceReady: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dashboard_service_ready",
			Help: "Whether the service answered /readyz with 200 (1) or not (0).",
		}, []string{"service"}),
		probeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "dashboard_probe_duration_seconds",
			Help:    "Latency of one /readyz probe, by service.",
			Buckets: prometheus.DefBuckets,
		}, []string{"service"}),
		upstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dashboard_upstream_requests_total",
			Help: "Upstream HTTP requests made, by upstream and result.",
		}, []string{"upstream", "result"}),
		upstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "dashboard_upstream_duration_seconds",
			Help:    "Latency of one upstream HTTP request, by upstream.",
			Buckets: prometheus.DefBuckets,
		}, []string{"upstream"}),
		cacheRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dashboard_cache_requests_total",
			Help: "Response-cache lookups, by endpoint and result (hit, miss or shared singleflight wait).",
		}, []string{"endpoint", "result"}),
		tailStreams: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dashboard_tail_streams",
			Help: "Open log-tail SSE streams holding a slot.",
		}),
		overviewDegraded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dashboard_overview_degraded_total",
			Help: "Overview cards degraded by a failed or empty query.",
		}, []string{"card"}),
	}

	m.buildInfo.WithLabelValues(version).Set(1)

	// Pre-initialise every closed label set at 0 so a scrape before the first event still
	// reports the series an alert is written against, and the series set can never grow
	// from a caller-supplied label.
	for _, upstream := range upstreamNames {
		m.upstreamDuration.WithLabelValues(upstream)
		for _, result := range upstreamResults {
			m.upstreamRequests.WithLabelValues(upstream, result)
		}
	}
	for _, endpoint := range cacheEndpoints {
		for _, result := range cacheResults {
			m.cacheRequests.WithLabelValues(endpoint, result)
		}
	}
	for _, q := range promql.Overview() {
		m.overviewDegraded.WithLabelValues(q.Card)
	}
	m.tailStreams.Set(0)

	reg.MustRegister(
		collectors.NewGoCollector(),
		m.requests,
		m.duration,
		m.tokenRejected,
		m.buildInfo,
		m.serviceUp,
		m.serviceReady,
		m.probeDuration,
		m.upstreamRequests,
		m.upstreamDuration,
		m.cacheRequests,
		m.tailStreams,
		m.overviewDegraded,
	)
	return m
}

// UpstreamRequest records one completed upstream HTTP request: its count in
// dashboard_upstream_requests_total and its latency in
// dashboard_upstream_duration_seconds. It implements the observers in prometheus, loki and
// auditsrc.
func (m *metrics) UpstreamRequest(upstream, result string, d time.Duration) {
	m.upstreamRequests.WithLabelValues(upstream, result).Inc()
	m.upstreamDuration.WithLabelValues(upstream).Observe(d.Seconds())
}

// CacheRequest records one response-cache lookup. It implements cache.Observer.
func (m *metrics) CacheRequest(endpoint, result string) {
	m.cacheRequests.WithLabelValues(endpoint, result).Inc()
}

// TailStreams adjusts the open-tail gauge. It implements api.Observer.
func (m *metrics) TailStreams(delta int) {
	m.tailStreams.Add(float64(delta))
}

// OverviewDegraded records one degraded overview card. It implements api.Observer.
func (m *metrics) OverviewDegraded(card string) {
	m.overviewDegraded.WithLabelValues(card).Inc()
}

// probeFinished records one completed probe. Up and ready are published as 0/1 gauges
// rather than a single enum so an alert can be written against either without parsing a
// reason string.
func (m *metrics) probeFinished(service string, up, ready bool, latency time.Duration) {
	m.serviceUp.WithLabelValues(service).Set(boolGauge(up))
	m.serviceReady.WithLabelValues(service).Set(boolGauge(ready))
	m.probeDuration.WithLabelValues(service).Observe(latency.Seconds())
}

// boolGauge renders a boolean as the 0/1 a Prometheus gauge expects.
func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// middleware records the request count and latency once the handler has returned, so a
// panicking request — which withRecover converts to a 500 rather than re-panicking — is
// still counted rather than vanishing from the metrics. It labels with routeLabel, the same
// value the access log uses, so logs and graphs can be read against each other.
//
// A request that is rejected for a missing or invalid token is labelled with the pattern of
// the route it was aimed at, not "unmatched": the token check runs after the mux has
// matched, so a 401 on a real route is attributed to that route, which is what makes "this
// route is rejecting everything" visible on a graph.
func (m *metrics) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recordWriter{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		route := routeLabel(r)
		m.requests.WithLabelValues(route, r.Method, statusLabel(status)).Inc()
		m.duration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}
