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
// status are their own labels, and nothing here carries a player id, a party id or a raw
// URL. Two labels sit outside that document's list and both are deliberate:
//
//   - `reason` on the rejection counter, which 05-gateway-techspec.md §8 defines for
//     Gateway, so an operator comparing the two services' rejection counts is reading
//     the same vocabulary on both;
//   - `domain` on the same counter, because this is the one service that verifies two
//     identity domains. "Everything on player routes is 401ing" and "everything on the
//     admin surface is 401ing" are different incidents with different first checks —
//     Auth's JWKS versus PHP Admin Auth's — and a single undifferentiated counter would
//     send an operator to look at both.
type metrics struct {
	registry      *prometheus.Registry
	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	tokenRejected *prometheus.CounterVec
	buildInfo     *prometheus.GaugeVec
	releaseChecks *prometheus.CounterVec
}

// newMetrics builds a fresh registry rather than registering on the global default one,
// so two servers in a single test binary do not collide and neither inherits whatever
// else has been registered process-wide. version is published as session_build_info; it
// is set once here and never updated, so a new build needs a restart to be reflected —
// which a deploy is.
func newMetrics(version string) *metrics {
	reg := prometheus.NewRegistry()

	m := &metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "session_http_requests_total",
			Help: "HTTP requests handled, by route, method and status.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "session_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by route and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		tokenRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "session_token_rejected_total",
			Help: "Tokens refused by this service, by identity domain and reason.",
		}, []string{"domain", "reason"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "session_build_info",
			Help: "Build information; the sample value is always 1.",
		}, []string{"version"}),
		releaseChecks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "session_release_checks_total",
			Help: "X-Otomo-Release checks on player routes, by result (current, outdated, missing, invalid, unchecked).",
		}, []string{"result"}),
	}

	m.buildInfo.WithLabelValues(version).Set(1)
	for _, r := range []string{releaseCurrent, releaseOutdated, releaseMissing, releaseInvalid, releaseUnchecked} {
		m.releaseChecks.WithLabelValues(r)
	}

	reg.MustRegister(
		collectors.NewGoCollector(),
		m.requests,
		m.duration,
		m.tokenRejected,
		m.buildInfo,
		m.releaseChecks,
	)
	return m
}

// middleware records the request count and latency once the handler has returned, so a
// panicking request — which withRecover converts to a 500 rather than re-panicking — is
// still counted rather than vanishing from the metrics. It labels with routeLabel, the
// same value the access log uses, so logs and graphs can be read against each other.
//
// A request that is rejected for a missing or invalid token is labelled with the pattern
// of the route it was aimed at, not "unmatched": the token check runs after the mux has
// matched, so a 401 on a real route is attributed to that route, which is what makes
// "this route is rejecting everything" visible on a graph.
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
