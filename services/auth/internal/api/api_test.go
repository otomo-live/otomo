package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/otomo-live/otomo/services/auth/internal/api"
)

type staticJWKS []byte

func (s staticJWKS) Bytes() []byte {
	return s
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not the COM-5 shape: %v", w.Body.String(), err)
	}
	return body.Error.Code
}

func TestWriteErrorShapeCarriesRequestID(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r = r.WithContext(api.WithRequestID(r.Context(), "req-123"))
	w := httptest.NewRecorder()

	api.WriteError(w, r, http.StatusTeapot, "teapot", "short and stout")

	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTeapot)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	if body.Error.Code != "teapot" {
		t.Errorf("code = %q, want teapot", body.Error.Code)
	}
	if body.Error.Message != "short and stout" {
		t.Errorf("message = %q", body.Error.Message)
	}
	if body.Error.RequestID != "req-123" {
		t.Errorf("request_id = %q, want req-123", body.Error.RequestID)
	}
}

func TestJWKSHandlerServesCachedBytes(t *testing.T) {
	raw := staticJWKS(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k1","x":"AAAA","use":"sig","alg":"EdDSA"}]}`)

	w := httptest.NewRecorder()
	api.JWKS(raw).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if w.Body.String() != string(raw) {
		t.Errorf("body = %q, want the cached bytes verbatim", w.Body.String())
	}
}

func TestStubsReturn501InCOM5Shape(t *testing.T) {
	for _, path := range []string{"/auth/anonymous", "/auth/refresh", "/auth/logout"} {
		w := httptest.NewRecorder()
		api.NotImplemented(w, httptest.NewRequest(http.MethodPost, path, nil))

		if w.Code != http.StatusNotImplemented {
			t.Errorf("POST %s status = %d, want 501", path, w.Code)
		}
		if code := errorCode(t, w); code != "not_implemented" {
			t.Errorf("POST %s code = %q, want not_implemented", path, code)
		}
	}
}

func TestHealthzAlwaysOK(t *testing.T) {
	w := httptest.NewRecorder()
	api.Healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", w.Body.String())
	}
}

func TestReadyz(t *testing.T) {
	ready := api.Readyz(func(context.Context) error { return nil })
	w := httptest.NewRecorder()
	ready.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	notReady := api.Readyz(func(context.Context) error { return errors.New("postgres unreachable") })
	w = httptest.NewRecorder()
	notReady.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if code := errorCode(t, w); code != "not_ready" {
		t.Errorf("code = %q, want not_ready", code)
	}
}

func TestFallback404And405InCOM5Shape(t *testing.T) {
	routes := []api.Route{
		{Method: http.MethodGet, Path: "/.well-known/jwks.json"},
		{Method: http.MethodPost, Path: "/auth/anonymous"},
	}
	handler := api.NotFoundOrMethodNotAllowed(routes)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown route status = %d, want 404", w.Code)
	}
	if code := errorCode(t, w); code != "not_found" {
		t.Errorf("unknown route code = %q, want not_found", code)
	}

	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/anonymous", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
	if code := errorCode(t, w); code != "method_not_allowed" {
		t.Errorf("wrong method code = %q, want method_not_allowed", code)
	}
}
