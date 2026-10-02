package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// servePublish runs the publish handler with no store for the given channel and body.
// Every case here is answered before the store would be consulted.
func servePublish(t *testing.T, ch, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, releasesPath, strings.NewReader(body))
	req.SetPathValue("ch", ch)
	w := httptest.NewRecorder()
	(&Handlers{}).For(Route{Method: http.MethodPost, Path: releasesPath}).ServeHTTP(w, req)
	return w
}

// TestPublishValidationNamesTheField is the "message names the field" contract for
// every rule §5 states. All of them are validation_failed, and the message must contain
// the field a form would highlight.
func TestPublishValidationNamesTheField(t *testing.T) {
	valid := `{"base_release_id":1,"versions":[],"packs":[],"min_client_version":"1.4.0","message":"ok"}`

	tests := []struct {
		name      string
		ch        string
		body      string
		wantField string
	}{
		{"bad channel", "prod", valid, "channel"},
		{"missing base", "dev", `{"versions":[],"packs":[],"min_client_version":"1.4.0","message":"ok"}`, "base_release_id"},
		{"base zero", "dev", `{"base_release_id":0,"versions":[],"packs":[],"min_client_version":"1.4.0","message":"ok"}`, "base_release_id"},
		{"bad min version", "dev", `{"base_release_id":1,"versions":[],"packs":[],"min_client_version":"1.4","message":"ok"}`, "min_client_version"},
		{"empty message", "dev", `{"base_release_id":1,"versions":[],"packs":[],"min_client_version":"1.4.0","message":"   "}`, "message"},
		{"long message", "dev", `{"base_release_id":1,"versions":[],"packs":[],"min_client_version":"1.4.0","message":"` + strings.Repeat("m", maxPublishMessage+1) + `"}`, "message"},
		{"missing versions", "dev", `{"base_release_id":1,"packs":[],"min_client_version":"1.4.0","message":"ok"}`, "versions"},
		{"missing packs", "dev", `{"base_release_id":1,"versions":[],"min_client_version":"1.4.0","message":"ok"}`, "packs"},
		{"duplicate namespace", "dev", `{"base_release_id":1,"versions":[{"namespace":"a","version":1},{"namespace":"a","version":2}],"packs":[],"min_client_version":"1.4.0","message":"ok"}`, "namespace"},
		{"bad pack sha", "dev", `{"base_release_id":1,"versions":[],"packs":[{"sha256":"XYZ"}],"min_client_version":"1.4.0","message":"ok"}`, "sha256"},
		{"duplicate pack sha", "dev", `{"base_release_id":1,"versions":[],"packs":[{"sha256":"` + strings.Repeat("a", 64) + `"},{"sha256":"` + strings.Repeat("a", 64) + `"}],"min_client_version":"1.4.0","message":"ok"}`, "sha256"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := servePublish(t, tt.ch, tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			code, msg := errorEnvelope(t, w)
			if code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
			if !strings.Contains(msg, tt.wantField) {
				t.Errorf("message %q does not name %q", msg, tt.wantField)
			}
		})
	}
}

// TestPublishAcceptsEmptyArraysAndChannels proves the two empty-array cases and each
// valid channel get past validation: with no store configured they answer 500, not 400.
func TestPublishAcceptsEmptyArraysAndChannels(t *testing.T) {
	for _, ch := range []string{"dev", "staging", "live"} {
		t.Run(ch, func(t *testing.T) {
			w := servePublish(t, ch, `{"base_release_id":1,"versions":[],"packs":[],"min_client_version":"1.4.0","message":"ok"}`)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
			}
			if code, _ := errorEnvelope(t, w); code != "internal_error" {
				t.Errorf("code = %q, want internal_error", code)
			}
		})
	}
}

// TestPublishRejectsUnknownFields pins the strict-reading rule: an unknown member is a
// 400 invalid_body rather than being ignored.
func TestPublishRejectsUnknownFields(t *testing.T) {
	w := servePublish(t, "dev", `{"base_release_id":1,"versions":[],"packs":[],"min_client_version":"1.4.0","message":"ok","extra":1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if code, _ := errorEnvelope(t, w); code != "invalid_body" {
		t.Errorf("code = %q, want invalid_body", code)
	}
}

// TestLivePublishRouteIsRegistered checks the literal live pattern resolves to the real
// handler rather than the 501 placeholder, so a live publish reaches publishRelease and
// its channel is implied by the path.
func TestLivePublishRouteIsRegistered(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, liveReleasesPath,
		strings.NewReader(`{"base_release_id":1,"versions":[],"packs":[],"min_client_version":"1.4.0","message":"ok"}`))
	w := httptest.NewRecorder()
	(&Handlers{}).For(Route{Method: http.MethodPost, Path: liveReleasesPath}).ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 from the real handler (%s)", w.Code, w.Body.String())
	}
}
