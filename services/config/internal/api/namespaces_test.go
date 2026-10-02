package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// createNamespaceHandler is the wired handler for POST /namespaces, without a store:
// enough for every case that must be answered before the store is consulted.
func createNamespaceHandler() http.Handler {
	return (&Handlers{}).For(Route{Method: http.MethodPost, Path: namespacesPath})
}

func serveCreate(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, namespacesPath, strings.NewReader(body))
	w := httptest.NewRecorder()
	createNamespaceHandler().ServeHTTP(w, req)
	return w
}

// errorEnvelope pulls the COM-5 code and message out of a response body.
func errorEnvelope(t *testing.T, w *httptest.ResponseRecorder) (code, message string) {
	t.Helper()

	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the COM-5 envelope: %v (%s)", err, w.Body.String())
	}
	return env.Error.Code, env.Error.Message
}

// TestValidateCreateNamespace covers every rule on both sides: a value at the limit
// and one just past it, an accepted enum and a rejected one. It tests the validator
// directly so a valid body can be asserted without a database.
func TestValidateCreateNamespace(t *testing.T) {
	name64 := "a" + strings.Repeat("b", 63)
	desc500 := strings.Repeat("d", 500)

	tests := []struct {
		name      string
		req       createNamespaceRequest
		wantField string // "" means the request is valid
	}{
		{"valid minimal", createNamespaceRequest{Name: "gameplay", Audience: "client"}, ""},
		{"valid dotted server", createNamespaceRequest{Name: "a.b_c.d9", Audience: "server"}, ""},
		{"valid name at 64", createNamespaceRequest{Name: name64, Audience: "client"}, ""},
		{"valid description at 500", createNamespaceRequest{Name: "gameplay", Audience: "client", Description: desc500}, ""},

		{"missing name", createNamespaceRequest{Audience: "client"}, "name"},
		{"name over 64", createNamespaceRequest{Name: name64 + "c", Audience: "client"}, "name"},
		{"name starts with digit", createNamespaceRequest{Name: "1game", Audience: "client"}, "name"},
		{"name uppercase", createNamespaceRequest{Name: "Game", Audience: "client"}, "name"},
		{"name hyphen", createNamespaceRequest{Name: "ga-me", Audience: "client"}, "name"},
		{"name empty segment", createNamespaceRequest{Name: "a..b", Audience: "client"}, "name"},
		{"name trailing dot", createNamespaceRequest{Name: "a.", Audience: "client"}, "name"},
		{"name leading dot", createNamespaceRequest{Name: ".a", Audience: "client"}, "name"},

		{"missing audience", createNamespaceRequest{Name: "gameplay"}, "audience"},
		{"unknown audience", createNamespaceRequest{Name: "gameplay", Audience: "both"}, "audience"},

		{"description over 500", createNamespaceRequest{Name: "gameplay", Audience: "client", Description: desc500 + "e"}, "description"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ve := validateCreate(tt.req)
			if tt.wantField == "" {
				if ve != nil {
					t.Fatalf("validateCreate(%+v) = %q, want valid", tt.req, ve.message)
				}
				return
			}
			if ve == nil {
				t.Fatalf("validateCreate(%+v) accepted the request, want a %s error", tt.req, tt.wantField)
			}
			if ve.field != tt.wantField {
				t.Errorf("field = %q, want %q (message %q)", ve.field, tt.wantField, ve.message)
			}
			if !strings.Contains(ve.message, tt.wantField) {
				t.Errorf("message %q does not name the field %q", ve.message, tt.wantField)
			}
		})
	}
}

// TestCreateNamespaceRejectsBadBodies is the HTTP half: malformed or ambiguous JSON
// is invalid_body, a well-formed body that breaks a field rule is validation_failed.
func TestCreateNamespaceRejectsBadBodies(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"malformed", `{`, "invalid_body"},
		{"empty", ``, "invalid_body"},
		{"unknown field", `{"name":"gameplay","audience":"client","extra":1}`, "invalid_body"},
		{"trailing data", `{"name":"gameplay","audience":"client"}{"x":1}`, "invalid_body"},
		{"trailing garbage", `{"name":"gameplay","audience":"client"} x`, "invalid_body"},
		{"too large", `{"name":"gameplay","audience":"client","description":"` + strings.Repeat("x", 70000) + `"}`, "invalid_body"},
		{"missing name", `{"audience":"client"}`, "validation_failed"},
		{"bad name", `{"name":"Game","audience":"client"}`, "validation_failed"},
		{"bad audience", `{"name":"gameplay","audience":"both"}`, "validation_failed"},
		{"description too long", `{"name":"gameplay","audience":"client","description":"` + strings.Repeat("x", 501) + `"}`, "validation_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveCreate(t, tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if code, _ := errorEnvelope(t, w); code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

// TestCreateNamespaceValidBodyReachesTheStore proves validation lets a good request
// through: with no store configured the handler answers 500, not 400, which is only
// possible after the body passed every rule.
func TestCreateNamespaceValidBodyReachesTheStore(t *testing.T) {
	w := serveCreate(t, `{"name":"gameplay","audience":"client"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if code, msg := errorEnvelope(t, w); code != "internal_error" || msg != "internal server error" {
		t.Errorf("error = %q/%q, want internal_error/internal server error", code, msg)
	}
}

// TestNilHandlersAnswer501 is the seam's contract with the existing placeholder tests:
// a Deps built without Handlers must still get a 501 on every route.
func TestNilHandlersAnswer501(t *testing.T) {
	var h *Handlers
	for _, rt := range Routes() {
		w := httptest.NewRecorder()
		h.For(rt).ServeHTTP(w, httptest.NewRequest(rt.Method, rt.Path, nil))
		if w.Code != http.StatusNotImplemented {
			t.Errorf("nil Handlers %s = %d, want 501", rt.Pattern(), w.Code)
		}
	}
}

// TestForFallsBackToNotImplemented covers an implemented Handlers with a pattern that
// has no entry in the map: only the routes registered in the map get real handlers.
func TestForFallsBackToNotImplemented(t *testing.T) {
	h := &Handlers{}
	rt := Route{Method: http.MethodGet, Path: "/api/admin/config/not-a-route"}

	w := httptest.NewRecorder()
	h.For(rt).ServeHTTP(w, httptest.NewRequest(http.MethodGet, rt.Path, nil))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}
