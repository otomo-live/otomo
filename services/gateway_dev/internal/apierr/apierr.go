package apierr

import (
	"context"
	"encoding/json"
	"net/http"
)

type requestIDKey struct{}

// WithRequestID stores the request ID on the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom retrieves the request ID from the context.
func RequestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// WriteError writes a COM-5 shaped error response:
// {"error":{"code":"...","message":"...","request_id":"..."}}.
func WriteError(w http.ResponseWriter, r *http.Request, statusCode int, code, message string) {
	body := errorBody{
		Error: errorDetail{
			Code:      code,
			Message:   message,
			RequestID: RequestIDFrom(r.Context()),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(body)
}
