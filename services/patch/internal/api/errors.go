// Package api holds the HTTP handlers and the wire formats they speak.
//
// Handlers are plain http.HandlerFunc constructors that take their dependencies as
// arguments, so the server package owns all routing and wiring and this package stays
// testable without a listener. Every 4xx and 5xx response uses the COM-5 error
// envelope defined here; no handler writes an error body by hand.
//
// Patch has no route table yet. This package exists now because Healthz, Readyz and
// the COM-5 envelope are shared with Config, and because the manifest and blob
// handlers PAT-B5 and PAT-A2 add later will use the same shape.
package api

import (
	"context"
	"encoding/json"
	"net/http"
)

// requestIDKey is the context key the request ID is stored under. It is an
// unexported struct type, so no importing package can collide with it.
type requestIDKey struct{}

// WithRequestID returns a copy of ctx carrying id. The server layer calls this once
// per request, which is what lets any handler below put the ID in an error body or a
// log line without threading it through as an argument.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the ID stored by WithRequestID, or "" when the context carries
// none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// errorBody is the COM-5 error envelope, the only error shape this service returns.
type errorBody struct {
	Error errorDetail `json:"error"`
}

// errorDetail is the envelope's inner object. Code is a stable, machine-readable
// token for clients to switch on; Message is for humans and may change.
type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// WriteError writes the COM-5 error envelope with the given HTTP status, code and
// message, filling request_id from the request context.
//
// It writes the header immediately, so a caller must not have written anything else
// to w first — status codes cannot be changed once the body has started.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{
		Code:      code,
		Message:   message,
		RequestID: RequestID(r.Context()),
	}})
}

// NotFound is the public listener's catch-all.
//
// Patch registers no routes yet: the manifest endpoint is PAT-B5 and blob delivery is
// nginx (PAT-A2), so at this point every public path is a COM-5 404. That is
// deliberate rather than an omission — a route that does not exist must say so in the
// one error shape clients already parse. It also means /healthz, /metrics and
// /debug/pprof/ return a plain 404 when asked for on the public port, without
// advertising that they exist on the internal one.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotFound, "not_found", "no such route")
}
