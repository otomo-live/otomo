// Cross-cutting middleware for the public listener: request IDs, structured access
// logging, request metrics and panic recovery. The wrapping order is set in
// publicHandler and is load-bearing; see the comment there.

package server

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/api"
)

// requestInfo carries per-request fields written by inner middleware and read by the
// access log. A pointer is stored on the context before the mux runs, so the token
// check — which happens inside the mux, per route — can record why it refused without
// the outer logging middleware needing to clone the request.
type requestInfo struct {
	Reason string
}

type requestInfoKey struct{}

// setReason records a token rejection reason on the per-request info. Safe to call when
// no info is on the context (the internal listener), which keeps every caller from
// having to check.
func setReason(ctx context.Context, reason string) {
	if info, ok := ctx.Value(requestInfoKey{}).(*requestInfo); ok {
		info.Reason = reason
	}
}

// recordWriter wraps a ResponseWriter to remember the status the handler chose, which
// both the access log and the metrics counter need and neither can get after the fact.
// It must stay a drop-in for http.ResponseWriter: handlers that type-assert for Flusher
// or reach for http.ResponseController rely on the methods below.
type recordWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader records only the first call and forwards every call, matching net/http's
// own behaviour — a handler that calls it twice gets a superfluous-call warning from the
// server, and the recorded status must be the one actually sent.
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

// Unwrap exposes the wrapped writer to http.ResponseController, which follows this
// method to reach optional interfaces such as Flusher and Hijacker.
func (w *recordWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// routeLabel returns the route pattern rather than the raw path — "/api/player/session/friends",
// not whatever the client sent. That keeps metrics cardinality bounded (an arbitrary
// path would mint a new time series per request) and keeps client-supplied path segments
// — player ids, party ids — out of the logs.
//
// The method is stripped because it is its own label, per COM-10. "unmatched" means the
// fallback handler served the request.
func routeLabel(r *http.Request) string {
	if r.Pattern == "" || r.Pattern == "/" {
		return "unmatched"
	}
	if i := strings.IndexByte(r.Pattern, ' '); i >= 0 {
		return r.Pattern[i+1:]
	}
	return r.Pattern
}

// withRequestID reuses an inbound X-Request-Id when Gateway supplied one, and mints a
// UUIDv7 (time-ordered, so a log sorted by ID is roughly sorted by time) when it did
// not. The value is echoed back on the response and put in the context, which is how it
// reaches both the error envelope and every log line for the request.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewV7().String()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(api.WithRequestID(r.Context(), id)))
	})
}

// withAccessLog writes one structured line per request, after the handler has returned
// so the status and duration are final. The raw path and query string are deliberately
// absent: they carry player and party identifiers, and routeLabel's pattern is what
// dashboards group on anyway.
//
// An event long-poll is logged when it *ends*, so its line appears up to 25 s after the
// request arrived and carries a ~25 s duration. That is correct and worth knowing before
// reading a log: a burst of 25 s lines is clients being served, not requests hanging.
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{}
		r = r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info))
		rec := &recordWriter{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "http_request",
			slog.String("method", r.Method),
			slog.String("route", routeLabel(r)),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)),
			slog.String("reason", info.Reason),
			slog.String("request_id", api.RequestID(r.Context())),
		)
	})
}

// withRecover turns a handler panic into a logged 500 in the COM-5 shape. The panic
// value and stack go to the log; the client is told only "internal server error", so an
// internal error message never leaks through a response body. Because it sits inside the
// log and metrics middleware, the failed request is still counted and still appears in
// the access log with its real 500.
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

// statusLabel renders a status code for the metrics label, which is a string so that the
// series is stable and readable rather than an integer that appears to be a value worth
// summing.
func statusLabel(status int) string {
	return strconv.Itoa(status)
}
