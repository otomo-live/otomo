package obslog

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/otomo-live/otomo/services/gateway_dev/internal/apierr"
)

// RequestInfo carries mutable per-request fields written by inner handlers
// and read by outer middleware. A pointer is stored on the context before the
// mux runs, so writes from inside the mux (group tagging now, GATE-3's
// rejection reason later) are visible to the logging middleware without the
// inner handler needing to clone the request.
type RequestInfo struct {
	Group  string // route group: "public", "player", "staff"
	Reason string // GATE-3: token rejection reason for metrics and logging
}

type requestInfoKey struct{}

func newRequestInfo(ctx context.Context) (context.Context, *RequestInfo) {
	info := &RequestInfo{}
	return context.WithValue(ctx, requestInfoKey{}, info), info
}

// RequestInfoFrom retrieves the per-request info pointer from the context.
// Returns nil if the logging middleware has not run (e.g. metrics listener).
func RequestInfoFrom(ctx context.Context) *RequestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*RequestInfo)
	return info
}

// SetGroup writes the route group on the per-request info. Safe to call when
// RequestInfo is nil (metrics listener paths).
func SetGroup(ctx context.Context, group string) {
	if info := RequestInfoFrom(ctx); info != nil {
		info.Group = group
	}
}

// SetReason writes the token rejection reason on the per-request info, so the
// access log line for a 401 says why. Same nil-safety as SetGroup.
func SetReason(ctx context.Context, reason string) {
	if info := RequestInfoFrom(ctx); info != nil {
		info.Reason = reason
	}
}

// routeLabel is the `route` value for logs and metrics: the path portion of the
// ServeMux pattern, with the method stripped, per COM-10. The method is its own
// label, so repeating it here would split one route's series in two — auth's
// middleware does the same stripping for the same reason.
func routeLabel(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}

// Init sets the default slog logger. JSON on stdout by default;
// set GATEWAY_DEV_LOG_PRETTY=1 for human-readable text (local dev only).
func Init() {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var h slog.Handler
	if os.Getenv("GATEWAY_DEV_LOG_PRETTY") == "1" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}

// RecoverMiddleware catches panics and writes a COM-5 500 response.
// Must be innermost (closest to the mux): RequestIDMiddleware has already set
// the ID, and LoggingMiddleware still writes its access log line after Recover
// returns normally — a panic must not skip the access log.
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.ErrorContext(r.Context(), "panic recovered",
					"error", v,
					"stack", string(debug.Stack()),
					"request_id", apierr.RequestIDFrom(r.Context()),
				)
				apierr.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// RequestIDMiddleware reads X-Request-Id from the inbound request or generates
// a v7 (time-ordered) UUID, then sets it on the context and response header.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewV7().String()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := apierr.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LoggingMiddleware emits one JSON log line per request: method, route pattern,
// status, duration, group, and request_id. It reads r.Pattern after
// next.ServeHTTP returns because ServeMux populates it at match time.
//
// On panic, this middleware's log line is skipped; RecoverMiddleware handles
// panic logging and response writing instead.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, info := newRequestInfo(r.Context())
		r = r.WithContext(ctx)
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		elapsed := time.Since(start)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		route := routeLabel(r.Pattern)
		slog.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.Int("status", status),
			slog.Duration("duration", elapsed),
			slog.String("group", info.Group),
			slog.String("reason", info.Reason),
			slog.String("request_id", apierr.RequestIDFrom(r.Context())),
		)
		HTTPRequestDuration.WithLabelValues(route, r.Method, strconv.Itoa(status)).Observe(elapsed.Seconds())
	})
}

// statusWriter captures the response status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.written {
		w.status = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.status = http.StatusOK
		w.written = true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.NewResponseController reach the underlying ResponseWriter
// (needed by GATE-2's stream handling for SetWriteDeadline/Flush).
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
