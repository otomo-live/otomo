package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveReplace runs the PUT schema handler with no store: every case here is answered
// before the store would be consulted.
func serveReplace(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPut, "/api/admin/config/namespaces/gameplay/schema", strings.NewReader(body))
	// The handler requires a precondition; these cases are all answered after it is
	// satisfied, by body size or compilation, before the store is consulted.
	r.Header.Set("If-Match", `"1"`)
	w := httptest.NewRecorder()
	(&Handlers{}).For(Route{Method: http.MethodPut, Path: schemasPath}).ServeHTTP(w, r)
	return w
}

// TestReplaceSchemaRejectsBadBodies checks the 413/400 split and that a rejected
// document never reaches the store.
func TestReplaceSchemaRejectsBadBodies(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"too large", `{"type":"object","description":"` + strings.Repeat("x", maxSchemaBody) + `"}`, http.StatusRequestEntityTooLarge, "body_too_large"},
		{"not json", `{`, http.StatusBadRequest, "validation_failed"},
		{"not an object", `[]`, http.StatusBadRequest, "validation_failed"},
		{"duplicate keys", `{"type":"object","type":"string"}`, http.StatusBadRequest, "validation_failed"},
		{"invalid schema", `{"type":12}`, http.StatusBadRequest, "validation_failed"},
		{"file reference", `{"$ref":"file:///etc/passwd"}`, http.StatusBadRequest, "validation_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveReplace(t, tt.body)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if code, _ := errorEnvelope(t, w); code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
			if strings.Contains(w.Body.String(), "root:") {
				t.Errorf("response leaked file contents: %s", w.Body.String())
			}
		})
	}
}

// TestReplaceSchemaValidBodyReachesTheStore proves a valid document passes compilation
// and is stopped only by the missing store: with no store configured the handler
// answers 500, not 400.
func TestReplaceSchemaValidBodyReachesTheStore(t *testing.T) {
	w := serveReplace(t, `{"type":"object"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if code, msg := errorEnvelope(t, w); code != "internal_error" || msg != "internal server error" {
		t.Errorf("error = %q/%q, want internal_error/internal server error", code, msg)
	}
}

// TestFirstLine bounds the compiler message the handler surfaces in the 400 body.
func TestFirstLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"single line", "boom", 300, "boom"},
		{"first of several", "first\nsecond", 300, "first"},
		{"truncated", strings.Repeat("x", 400), 300, strings.Repeat("x", 300)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstLine(tt.in, tt.max); got != tt.want {
				t.Errorf("firstLine = %q, want %q", got, tt.want)
			}
		})
	}
}
