package server

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics holds the service's Prometheus instruments and the registry they are
// registered on.
//
// Labels follow COM-10: route is the mux pattern with the method stripped, method and
// status are their own labels, and nothing here carries a caller identifier or a raw
// URL. Later tickets register their own collectors — registry sizes, allocation
// counts, reservation ages — through Server.Registry rather than adding package-level
// variables here, so the registry stays owned by one server instance.
type metrics struct {
	registry  *prometheus.Registry
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	buildInfo *prometheus.GaugeVec
}

// newMetrics builds a fresh registry rather than registering on the global default
// one, so two servers in a single test binary do not collide and neither inherits
// whatever else has been registered process-wide. version is published as
// allocator_build_info; it is set once here and never updated, so a new build needs a
// restart to be reflected — which a deploy is.
func newMetrics(version string) *metrics {
	reg := prometheus.NewRegistry()

	m := &metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "allocator_http_requests_total",
			Help: "HTTP requests handled, by route, method and status.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "allocator_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by route and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "allocator_build_info",
			Help: "Build information; the sample value is always 1.",
		}, []string{"version"}),
	}
	m.buildInfo.WithLabelValues(version).Set(1)

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests,
		m.duration,
		m.buildInfo,
	)
	return m
}

// middleware records the request count and latency once the handler has returned, so a
// panicking request — which withRecover converts to a 500 rather than re-panicking — is
// still counted rather than vanishing from the metrics. It labels with routeLabel, the
// same value the access log uses, so logs and graphs can be read against each other.
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
