// Cross-cutting middleware for the public listener: request IDs, structured access
// logging, request metrics and panic recovery. The wrapping order is set in
// publicHandler and is load-bearing; see the comment there.

package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/otomo-live/otomo/services/auth/internal/api"
)

// recordWriter wraps a ResponseWriter to remember the status the handler chose, which
// both the access log and the metrics counter need and neither can get after the
// fact. It must stay a drop-in for http.ResponseWriter: handlers that type-assert for
// Flusher or reach for http.ResponseController rely on the methods below.
type recordWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader records only the first call and forwards every call, matching
// net/http's own behaviour — a handler that calls it twice gets a superfluous-call
// warning from the server, and the recorded status must be the one actually sent.
func (w *recordWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write covers the handler that writes a body without ever calling WriteHeader, which
// net/http turns into an implicit 200.
func (w *recordWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the wrapped writer when it can flush, so streaming and SSE
// responses still reach the client as they are written instead of being buffered by
// the wrapper.
func (w *recordWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController, which follows this
// method to reach optional interfaces such as Flusher and Hijacker.
func (w *recordWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// routeLabel returns the route pattern rather than the raw path — "/auth/anonymous",
// not whatever the client sent. That keeps metrics cardinality bounded (an arbitrary
// path would mint a new time series per request) and keeps client-supplied path
// segments out of the logs. "unmatched" means the fallback handler served it.
func routeLabel(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	if i := strings.IndexByte(r.Pattern, ' '); i >= 0 {
		return r.Pattern[i+1:]
	}
	return r.Pattern
}

// withRequestID reuses an inbound X-Request-Id when Gateway supplied one, and mints a
// UUID when it did not, so a request that originates outside the mesh still gets an
// ID. The value is echoed back on the response and put in the context, which is how
// it reaches both the error envelope and every log line for the request.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.New().String()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(api.WithRequestID(r.Context(), id)))
	})
}

// withAccessLog writes one structured line per request, after the handler has
// returned so the status and duration are final. The raw path and query string are
// deliberately absent: they can carry player identifiers, and routeLabel's pattern is
// what dashboards group on anyway.
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recordWriter{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "http_request",
			slog.String("method", r.Method),
			slog.String("route", routeLabel(r)),
			slog.Int("status", rec.status),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			slog.String("request_id", api.RequestID(r.Context())),
		)
	})
}

// withRecover turns a handler panic into a logged 500 in the COM-5 shape. The panic
// value and stack go to the log; the client is told only "internal server error", so
// an internal error message never leaks through a response body. Because it sits
// inside the log and metrics middleware, the failed request is still counted and
// still appears in the access log with its real 500.
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			s.log.LogAttrs(r.Context(), slog.LevelError, "handler_panic",
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())),
				slog.String("request_id", api.RequestID(r.Context())),
			)
			api.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// metrics holds the service's Prometheus instruments and the registry they are
// registered on.
type metrics struct {
	registry  *prometheus.Registry
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	jwksKeys  prometheus.Gauge
	buildInfo *prometheus.GaugeVec

	// AU-6: what each login and refresh ended in. Neither carries anything about the
	// caller: no device, account or token ever becomes a label.
	logins  *prometheus.CounterVec
	refresh *prometheus.CounterVec
}

// Login counts one POST /auth/anonymous (api.Outcomes).
func (m *metrics) Login(result string, newAccount bool) {
	m.logins.WithLabelValues(result, strconv.FormatBool(newAccount)).Inc()
}

// Refresh counts one POST /auth/refresh (api.Outcomes).
func (m *metrics) Refresh(result string) {
	m.refresh.WithLabelValues(result).Inc()
}

// newMetrics builds a fresh registry rather than registering on the global default
// one, so two servers in a single test binary do not collide and neither inherits
// whatever else has been registered process-wide. version and jwksKeys are published
// as the auth_build_info and auth_jwks_keys_active gauges; both are set once here and
// never updated, so a key rotation needs a restart to be reflected.
func newMetrics(version string, jwksKeys int) *metrics {
	reg := prometheus.NewRegistry()

	m := &metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "auth_http_requests_total",
			Help: "HTTP requests handled, by route, method and status.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "auth_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by route and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		jwksKeys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "auth_jwks_keys_active",
			Help: "Signing keys currently published as active in the JWKS.",
		}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "auth_build_info",
			Help: "Build information; the sample value is always 1.",
		}, []string{"version"}),
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "auth_logins_total",
			Help: "Device logins, by result (ok, invalid, error) and whether the login created the account.",
		}, []string{"result", "new_account"}),
		refresh: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "auth_refresh_total",
			Help: "Refresh token exchanges, by result (ok, invalid, revoked, reuse_detected, error).",
		}, []string{"result"}),
	}

	m.jwksKeys.Set(float64(jwksKeys))
	m.buildInfo.WithLabelValues(version).Set(1)
	// Every series exists from the first scrape, so a rate() or an alert on
	// reuse_detected has a zero to start from rather than no data.
	for _, r := range []string{api.ResultOK, api.ResultInvalid, api.ResultError} {
		for _, n := range []string{"true", "false"} {
			m.logins.WithLabelValues(r, n)
		}
	}
	for _, r := range []string{api.ResultOK, api.ResultInvalid, api.ResultRevoked, api.ResultReuseDetected, api.ResultError} {
		m.refresh.WithLabelValues(r)
	}

	reg.MustRegister(
		collectors.NewGoCollector(),
		m.requests,
		m.duration,
		m.jwksKeys,
		m.buildInfo,
		m.logins,
		m.refresh,
	)
	return m
}

// middleware records the request count and latency once the handler has returned, so
// a panicking request — which withRecover converts to a 500 rather than re-panicking
// — is still counted rather than vanishing from the metrics. It labels with
// routeLabel, the same value the access log uses, so logs and graphs can be read
// against each other.
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
		m.requests.WithLabelValues(route, r.Method, strconv.Itoa(status)).Inc()
		m.duration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}
