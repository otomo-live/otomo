// Cross-cutting middleware for the API listener: request IDs, structured access
// logging, request metrics and panic recovery. The wrapping order is set in apiHandler
// and is load-bearing; see the comment there.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/otomo-live/otomo/services/allocator/internal/api"
)

// requestIDHeader is where the request ID is read from and echoed to. HTTP header
// names are case-insensitive, so the mixed-case spelling here is the one Go
// canonicalizes to and the one clients will see.
const requestIDHeader = "X-Request-ID"

// maxRequestIDLen bounds an inbound request ID so a caller cannot push an arbitrarily
// long value into every log line for the request.
const maxRequestIDLen = 128

// recordWriter wraps a ResponseWriter to remember the status the handler chose, which
// both the access log and the metrics counter need and neither can get after the fact.
// It must stay a drop-in for http.ResponseWriter: handlers that type-assert for
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

// Unwrap exposes the wrapped writer to http.ResponseController, which follows this
// method to reach optional interfaces such as Flusher and Hijacker.
func (w *recordWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// routeLabel returns the route pattern rather than the raw path — "/allocator/v1/servers",
// not whatever the client sent. That keeps metrics cardinality bounded (an arbitrary
// path would mint a new time series per request) and keeps client-supplied path
// segments out of the logs.
//
// The method is stripped because it is its own label, per COM-10. "" and "/" both mean
// the fallback handler served the request: ServeMux records the catch-all's pattern on
// the request, so treating "/" as a real route would label every unknown path as if it
// were a registered one.
func routeLabel(r *http.Request) string {
	if r.Pattern == "" || r.Pattern == "/" {
		return "unmatched"
	}
	if i := strings.IndexByte(r.Pattern, ' '); i >= 0 {
		return r.Pattern[i+1:]
	}
	return r.Pattern
}

// withRequestID reuses an inbound request ID when Gateway supplied one, and mints a
// random hex ID when it did not. The value is echoed back on the response and put in
// the context, which is how it reaches both the error envelope and every log line for
// the request.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(api.WithRequestID(r.Context(), id)))
	})
}

// validRequestID accepts an inbound ID only if it is 1 to 128 printable ASCII bytes.
// Anything else is replaced rather than rejected, so a client that sends a broken ID
// still gets a usable request rather than a 400 over a log field. Restricting it to
// printable ASCII is what keeps a newline or a control character out of the access
// log, where it could forge a second line or a second field.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// newRequestID mints a 128-bit ID as 32 hex characters. It is deliberately random
// rather than sequential: an ID that encodes time or a counter would let a caller
// guess another request's ID and correlate its logs.
func newRequestID() string {
	var b [16]byte
	// crypto/rand.Read is documented never to fail on a supported platform; the
	// fallback exists so the header is never empty even if that changes.
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// withAccessLog writes one structured line per request, after the handler has returned
// so the status and duration are final. The raw path and query string are deliberately
// absent: they can carry a server ID or a party token, and routeLabel's pattern is what
// dashboards group on anyway.
//
// The Authorization header is never logged, here or anywhere else in this service.
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
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.String("request_id", api.RequestID(r.Context())),
		)
	})
}

// withRecover turns a handler panic into a logged 500 in the COM-5 shape. The panic
// value and stack go to the log; the client is told only "internal server error", so an
// internal error message never leaks through a response body. Because it sits inside
// the log and metrics middleware, the failed request is still counted and still appears
// in the access log with its real 500.
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

// statusLabel renders a status code for the metrics label, which is a string so that
// the series is stable and readable rather than an integer that appears to be a value
// worth summing.
func statusLabel(status int) string {
	return strconv.Itoa(status)
}
