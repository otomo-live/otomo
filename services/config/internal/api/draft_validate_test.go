package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveValidateDraft runs the validate handler with no store: every case here is
// answered before the store would be consulted.
func serveValidateDraft(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/api/admin/config/namespaces/gameplay/draft/validate", strings.NewReader(body))
	w := httptest.NewRecorder()
	(&Handlers{}).For(Route{Method: http.MethodPost, Path: draftValidatePath}).ServeHTTP(w, r)
	return w
}

// TestValidateDraftRejectsBadBodies reuses the draft PUT's body rules, so the 413/400
// split and the invalid_body/validation_failed split must match exactly.
func TestValidateDraftRejectsBadBodies(t *testing.T) {
	tooLarge := `{"document":{"x":"` + strings.Repeat("x", maxDraftBody) + `"}}`

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"too large", tooLarge, http.StatusRequestEntityTooLarge, "body_too_large"},
		{"malformed", `{`, http.StatusBadRequest, "invalid_body"},
		{"unknown field", `{"document":{},"revision":1}`, http.StatusBadRequest, "invalid_body"},
		{"trailing data", `{"document":{}}{"x":1}`, http.StatusBadRequest, "invalid_body"},
		{"duplicate key", `{"document":{},"document":{}}`, http.StatusBadRequest, "invalid_body"},

		{"missing document", `{}`, http.StatusBadRequest, "validation_failed"},
		{"document is array", `{"document":[]}`, http.StatusBadRequest, "validation_failed"},
		{"document is string", `{"document":"x"}`, http.StatusBadRequest, "validation_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveValidateDraft(t, tt.body)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if code, _ := errorEnvelope(t, w); code != tt.wantCode {
				t.Errorf("code = %q, want %q (%s)", code, tt.wantCode, w.Body.String())
			}
		})
	}
}

// TestValidateDraftValidBodyReachesTheStore proves the body rules let a good request
// through: with no store configured the handler answers 500, not 400.
func TestValidateDraftValidBodyReachesTheStore(t *testing.T) {
	w := serveValidateDraft(t, `{"document":{"weapons":[{"damage":3}]}}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if code, _ := errorEnvelope(t, w); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
}
