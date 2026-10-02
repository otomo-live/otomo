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
// status are their own labels, and nothing here carries a namespace name, a staff
// identifier or a raw URL. `reason` on the rejection counter is the one label outside
// that document's list, and it is there because 05-gateway-techspec.md §8 defines the
// same series for Gateway: an operator comparing the two services' rejection counts
// should be reading the same vocabulary on both.
type metrics struct {
	registry        *prometheus.Registry
	requests        *prometheus.CounterVec
	duration        *prometheus.HistogramVec
	tokenRejected   *prometheus.CounterVec
	buildInfo       *prometheus.GaugeVec
	packUploadBytes prometheus.Counter
	publishTotal    *prometheus.CounterVec
	validationFails prometheus.Counter
}

// newMetrics builds a fresh registry rather than registering on the global default
// one, so two servers in a single test binary do not collide and neither inherits
// whatever else has been registered process-wide. version is published as
// config_build_info; it is set once here and never updated, so a new build needs a
// restart to be reflected — which a deploy is.
func newMetrics(version string) *metrics {
	reg := prometheus.NewRegistry()

	m := &metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "config_http_requests_total",
			Help: "HTTP requests handled, by route, method and status.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "config_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by route and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		tokenRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "config_token_rejected_total",
			Help: "Staff tokens refused by this service, by reason.",
		}, []string{"reason"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "config_build_info",
			Help: "Build information; the sample value is always 1.",
		}, []string{"version"}),
		packUploadBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "config_pack_upload_bytes_total",
			Help: "Bytes of content packs accepted by successful uploads.",
		}),
		publishTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "config_publish_total",
			Help: "Successful publishes, by channel.",
		}, []string{"channel"}),
		validationFails: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "config_validation_failures_total",
			Help: "Documents found invalid against their namespace schema.",
		}),
	}

	m.buildInfo.WithLabelValues(version).Set(1)

	// Touch every channel so the series exist at 0 before the first publish: an
	// operator can alert on the series without waiting for one to be created.
	for _, ch := range []string{"dev", "staging", "live"} {
		m.publishTotal.WithLabelValues(ch)
	}

	reg.MustRegister(
		collectors.NewGoCollector(),
		m.requests,
		m.duration,
		m.tokenRejected,
		m.buildInfo,
		m.packUploadBytes,
		m.publishTotal,
		m.validationFails,
	)
	return m
}

// middleware records the request count and latency once the handler has returned, so a
// panicking request — which withRecover converts to a 500 rather than re-panicking — is
// still counted rather than vanishing from the metrics. It labels with routeLabel, the
// same value the access log uses, so logs and graphs can be read against each other.
//
// A request that is rejected for a missing or invalid token is labelled with the
// pattern of the route it was aimed at, not "unmatched": the token check runs after
// the mux has matched, so a 401 on a real route is attributed to that route, which is
// what makes "this route is rejecting everything" visible on a graph.
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
