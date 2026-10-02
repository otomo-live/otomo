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
	registry       *prometheus.Registry
	requests       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	tokenRejected  *prometheus.CounterVec
	buildInfo      *prometheus.GaugeVec
	currentRelease *prometheus.GaugeVec
	manifestReload *prometheus.CounterVec
	manifests      *prometheus.CounterVec
	blobs          *prometheus.CounterVec
}

// newMetrics builds a fresh registry rather than registering on the global default
// one, so two servers in a single test binary do not collide and neither inherits
// whatever else has been registered process-wide. version is published as
// patch_build_info; it is set once here and never updated, so a new build needs a
// restart to be reflected — which a deploy is.
func newMetrics(version string) *metrics {
	reg := prometheus.NewRegistry()

	m := &metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patch_http_requests_total",
			Help: "HTTP requests handled, by route, method and status.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patch_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by route and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		tokenRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patch_token_rejected_total",
			Help: "Staff tokens refused by this service, by reason.",
		}, []string{"reason"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "patch_build_info",
			Help: "Build information; the sample value is always 1.",
		}, []string{"version"}),
		currentRelease: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "patch_current_release_id",
			Help: "Release ID currently loaded per channel.",
		}, []string{"channel"}),
		manifestReload: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patch_manifest_reload_total",
			Help: "Manifest reloads published, by source (notify, poll or reconnect).",
		}, []string{"source"}),
		manifests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patch_manifest_requests_total",
			Help: "Manifest responses served, by channel and result (200 or 304).",
		}, []string{"channel", "result"}),
		blobs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patch_blob_requests_total",
			Help: "Blob responses served, by result status (200, 206, 304, 404 or 400).",
		}, []string{"result"}),
	}

	m.buildInfo.WithLabelValues(version).Set(1)

	reg.MustRegister(
		collectors.NewGoCollector(),
		m.requests,
		m.duration,
		m.tokenRejected,
		m.buildInfo,
		m.currentRelease,
		m.manifestReload,
		m.manifests,
		m.blobs,
	)
	return m
}

// ManifestRequest implements api.ManifestObserver. It counts only the results that
// carried a manifest or a revalidation: a 401, 403, 404 or 503 is a different series,
// and mixing them in would make the 304 ratio meaningless.
func (m *metrics) ManifestRequest(channel, result string) {
	m.manifests.WithLabelValues(channel, result).Inc()
}

// TokenRejected implements api.ManifestObserver and reuses the same
// patch_token_rejected_total series every other token refusal in this service records
// to, so one graph covers all of them.
func (m *metrics) TokenRejected(reason string) {
	m.tokenRejected.WithLabelValues(reason).Inc()
}

// BlobRequest implements api.BlobObserver. The result is the HTTP status the handler
// actually wrote, so 200, 206, 304, 404 and 400 each get their own series.
func (m *metrics) BlobRequest(result string) {
	m.blobs.WithLabelValues(result).Inc()
}

// RecordManifestReload counts one manifest reload by the source that triggered it:
// "notify" for a LISTEN/NOTIFY message, "poll" for the fallback ticker and
// "reconnect" for the LoadAll that follows a listener (re)connect.
func (s *Server) RecordManifestReload(source string) {
	s.metrics.manifestReload.WithLabelValues(source).Inc()
}

// SetCurrentRelease records the release a channel was last loaded at. It is called for
// every entry of every published Set, so the gauge tracks what Patch is actually
// serving rather than what channel_head currently points at.
func (s *Server) SetCurrentRelease(channel string, releaseID int64) {
	s.metrics.currentRelease.WithLabelValues(channel).Set(float64(releaseID))
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
