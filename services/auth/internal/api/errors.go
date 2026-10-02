// Package api holds the HTTP handlers and the wire formats they speak.
//
// Handlers are plain http.HandlerFunc constructors that take their dependencies as
// arguments, so the server package owns all routing and wiring and this package stays
// testable without a listener. Every 4xx and 5xx response uses the COM-5 error
// envelope defined here; no handler writes an error body by hand.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

// Route is one method/path pair the service serves. The server package passes its
// route table here so the fallback handler can tell a request for the wrong method on
// a known path (405) from a request for a path that does not exist at all (404).
type Route struct {
	Method string
	Path   string
}

// NotFoundOrMethodNotAllowed returns the fallback handler for routes the router did
// not match. It answers 405 with an Allow header listing the methods that path does
// support, or 404 when the path is unknown to any method. Both use the COM-5 shape —
// notably, a request for an internal-only path such as /metrics lands here and gets a
// plain 404, which does not advertise that the endpoint exists elsewhere.
func NotFoundOrMethodNotAllowed(routes []Route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var allowed []string
		for _, rt := range routes {
			if rt.Path == r.URL.Path {
				allowed = append(allowed, rt.Method)
			}
		}
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this path")
			return
		}
		WriteError(w, r, http.StatusNotFound, "not_found", "no such route")
	}
}
