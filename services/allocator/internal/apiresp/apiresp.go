// Package apiresp holds the COM-5 response envelope and the request-ID plumbing the
// API handlers and the service-key middleware both need.
//
// It is a leaf package on purpose. The api package depends on servicekey to guard its
// routes, and servicekey writes the same 401/403 envelope api does, so keeping the
// envelope in api would make the two packages import each other. Keeping it here lets
// api own the routes and servicekey own the guards without either inventing a second
// error shape.
package apiresp

import (
	"context"
	"encoding/json"
	"net/http"
)

// requestIDKey is the context key the request ID is stored under. It is an unexported
// struct type, so no importing package can collide with it.
type requestIDKey struct{}

// WithRequestID returns a copy of ctx carrying id. The server layer calls this once per
// request, which is what lets any handler put the ID in an error body or a log line
// without threading it through as an argument.
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

// WriteJSON writes v as a JSON body with the given status. It exists so every handler
// and the error envelope share one place that sets Content-Type and the status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the COM-5 error envelope with the given HTTP status, code and
// message, filling request_id from the request context.
//
// It writes the header immediately, so a caller must not have written anything else
// to w first — status codes cannot be changed once the body has started.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	WriteJSON(w, status, errorBody{Error: errorDetail{
		Code:      code,
		Message:   message,
		RequestID: RequestID(r.Context()),
	}})
}
