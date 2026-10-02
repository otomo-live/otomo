// Package api holds the HTTP handlers and the wire formats they speak.
//
// Handlers are plain http.HandlerFunc constructors that take their dependencies as
// arguments, so the server package owns all routing and wiring and this package stays
// testable without a listener. Every 4xx and 5xx response uses the COM-5 error
// envelope; no handler writes an error body by hand.
//
// The envelope itself lives in the leaf package apiresp, because servicekey writes the
// same 401/403 bodies and api also imports servicekey to guard its routes: keeping the
// implementation here would be an import cycle. The functions below are thin wrappers
// so existing callers keep saying api.WriteError, and there is still one place that
// defines the shape.
package api

import (
	"context"
	"net/http"

	"github.com/otomo-live/otomo/services/allocator/internal/apiresp"
)

// WithRequestID returns a copy of ctx carrying id. The server layer calls this once
// per request, which is what lets any handler below put the ID in an error body or a
// log line without threading it through as an argument.
func WithRequestID(ctx context.Context, id string) context.Context {
	return apiresp.WithRequestID(ctx, id)
}

// RequestID returns the ID stored by WithRequestID, or "" when the context carries
// none.
func RequestID(ctx context.Context) string {
	return apiresp.RequestID(ctx)
}

// WriteJSON writes v as a JSON body with the given status. It exists so the health
// handlers and the error envelope share one place that sets Content-Type and the
// status, and so a handler has a single obvious way to write a success body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	apiresp.WriteJSON(w, status, v)
}

// WriteError writes the COM-5 error envelope with the given HTTP status, code and
// message, filling request_id from the request context.
//
// It writes the header immediately, so a caller must not have written anything else
// to w first — status codes cannot be changed once the body has started.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	apiresp.WriteError(w, r, status, code, message)
}

// NotFound is the API listener's catch-all.
//
// The registry routes live in servers.go; everything else on the API port is a COM-5
// 404. That is deliberate rather than an omission — a route that does not exist must
// say so in the one error shape callers already parse. It also means /healthz,
// /metrics and /debug/pprof/ return a plain 404 when asked for on the API port,
// without advertising that they exist on the metrics one.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotFound, "not_found", "no such route")
}
