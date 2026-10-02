package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveSaveDraft runs the PUT draft handler with no store: every case here is answered
// before the store would be consulted.
func serveSaveDraft(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPut, "/api/admin/config/namespaces/gameplay/draft", strings.NewReader(body))
	w := httptest.NewRecorder()
	(&Handlers{}).For(Route{Method: http.MethodPut, Path: draftsPath}).ServeHTTP(w, r)
	return w
}

// TestSaveDraftRejectsBadBodies checks the 413/400 split and the two 400 codes: a body
// that is not valid JSON (including the duplicate keys a plain decoder would accept) is
// invalid_body, a well-formed body that breaks a field rule is validation_failed.
func TestSaveDraftRejectsBadBodies(t *testing.T) {
	tooLarge := `{"document":{"x":"` + strings.Repeat("x", maxDraftBody) + `"},"revision":1}`

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"too large", tooLarge, http.StatusRequestEntityTooLarge, "body_too_large"},
		{"malformed", `{`, http.StatusBadRequest, "invalid_body"},
		{"unknown field", `{"document":{},"revision":1,"extra":1}`, http.StatusBadRequest, "invalid_body"},
		{"trailing data", `{"document":{},"revision":1}{"x":1}`, http.StatusBadRequest, "invalid_body"},
		{"duplicate key", `{"document":{},"document":{},"revision":1}`, http.StatusBadRequest, "invalid_body"},

		{"missing document", `{"revision":1}`, http.StatusBadRequest, "validation_failed"},
		{"document is array", `{"document":[],"revision":1}`, http.StatusBadRequest, "validation_failed"},
		{"document is string", `{"document":"x","revision":1}`, http.StatusBadRequest, "validation_failed"},
		{"missing revision", `{"document":{}}`, http.StatusBadRequest, "validation_failed"},
		{"revision zero", `{"document":{},"revision":0}`, http.StatusBadRequest, "validation_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveSaveDraft(t, tt.body)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if code, _ := errorEnvelope(t, w); code != tt.wantCode {
				t.Errorf("code = %q, want %q (%s)", code, tt.wantCode, w.Body.String())
			}
		})
	}
}

// TestSaveDraftValidationNamesTheField is the "message names the field" contract: a
// client highlighting a form field should not have to parse prose.
func TestSaveDraftValidationNamesTheField(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"missing document", `{"revision":1}`, "document"},
		{"document not an object", `{"document":[],"revision":1}`, "document"},
		{"missing revision", `{"document":{}}`, "revision"},
		{"revision zero", `{"document":{},"revision":0}`, "revision"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveSaveDraft(t, tt.body)
			if _, msg := errorEnvelope(t, w); !strings.Contains(msg, tt.wantField) {
				t.Errorf("message %q does not name %q", msg, tt.wantField)
			}
		})
	}
}

// TestSaveDraftValidBodyReachesTheStore proves validation lets a good request through:
// with no store configured the handler answers 500, not 400, which is only possible
// after the body passed every rule.
func TestSaveDraftValidBodyReachesTheStore(t *testing.T) {
	w := serveSaveDraft(t, `{"document":{"level":1},"revision":1}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if code, msg := errorEnvelope(t, w); code != "internal_error" || msg != "internal server error" {
		t.Errorf("error = %q/%q, want internal_error/internal server error", code, msg)
	}
}

// TestGetDraftNoStoreIsAnInternalError pins the read handler's fail-closed direction.
func TestGetDraftNoStoreIsAnInternalError(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/admin/config/namespaces/gameplay/draft", nil)
	w := httptest.NewRecorder()
	(&Handlers{}).For(Route{Method: http.MethodGet, Path: draftsPath}).ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
}
