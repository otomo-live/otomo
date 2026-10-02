package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveRelease(t *testing.T, method, routePath, target, ch, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if ch != "" {
		req.SetPathValue("ch", ch)
	}
	w := httptest.NewRecorder()
	(&Handlers{}).For(Route{Method: method, Path: routePath}).ServeHTTP(w, req)
	return w
}

// TestListReleasesValidation covers the request rules answered before the store is
// consulted: a bad channel, a non-positive before and an out-of-range limit.
func TestListReleasesValidation(t *testing.T) {
	tests := []struct {
		name string
		ch   string
		url  string
	}{
		{"unknown channel", "prod", releasesPath},
		{"bad before", "dev", releasesPath + "?before=zero"},
		{"zero before", "dev", releasesPath + "?before=0"},
		{"bad limit", "dev", releasesPath + "?limit=0"},
		{"limit too high", "dev", releasesPath + "?limit=201"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveRelease(t, http.MethodGet, releasesPath, tt.url, tt.ch, "")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if code, _ := errorEnvelope(t, w); code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
		})
	}

	// A valid request with no store is a 500, not a 400.
	if w := serveRelease(t, http.MethodGet, releasesPath, releasesPath, "dev", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("valid request without a store = %d, want 500", w.Code)
	}
}

// TestRollbackValidation covers the two body rules and the strict-body rule.
func TestRollbackValidation(t *testing.T) {
	tests := []struct {
		name string
		ch   string
		body string
	}{
		{"unknown channel", "prod", `{"release_id":1,"base_release_id":2}`},
		{"missing release", "dev", `{"base_release_id":2}`},
		{"zero release", "dev", `{"release_id":0,"base_release_id":2}`},
		{"missing base", "dev", `{"release_id":1}`},
		{"zero base", "dev", `{"release_id":1,"base_release_id":0}`},
		{"unknown field", "dev", `{"release_id":1,"base_release_id":2,"extra":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveRelease(t, http.MethodPost, rollbackPath, rollbackPath, tt.ch, tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
		})
	}

	if w := serveRelease(t, http.MethodPost, rollbackPath, rollbackPath, "dev", `{"release_id":1,"base_release_id":2}`); w.Code != http.StatusInternalServerError {
		t.Errorf("valid rollback without a store = %d, want 500", w.Code)
	}
}

// TestPromoteLadderValidation is the ladder rule: from must be the channel directly
// below ch, so dev has no source and live only accepts staging.
func TestPromoteLadderValidation(t *testing.T) {
	body := `{"base_release_id":1}`
	tests := []struct {
		name string
		ch   string
		from string
		want int
	}{
		{"dev has no lower channel", "dev", "staging", http.StatusBadRequest},
		{"live cannot promote from dev", "live", "dev", http.StatusBadRequest},
		{"staging cannot promote from live", "staging", "live", http.StatusBadRequest},
		{"missing from", "live", "", http.StatusBadRequest},
		{"valid staging", "staging", "dev", http.StatusInternalServerError},
		{"valid live", "live", "staging", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, promotePath+"?from="+tt.from, strings.NewReader(body))
			req.SetPathValue("ch", tt.ch)
			w := httptest.NewRecorder()
			(&Handlers{}).For(Route{Method: http.MethodPost, Path: promotePath}).ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tt.want, w.Body.String())
			}
			if tt.want == http.StatusBadRequest {
				if code, _ := errorEnvelope(t, w); code != "validation_failed" {
					t.Errorf("code = %q, want validation_failed", code)
				}
			}
		})
	}
}

// TestPromoteBodyValidation covers base_release_id and the optional message.
func TestPromoteBodyValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing base", `{}`},
		{"zero base", `{"base_release_id":0}`},
		{"long message", `{"base_release_id":1,"message":"` + strings.Repeat("m", maxPublishMessage+1) + `"}`},
		{"unknown field", `{"base_release_id":1,"extra":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, promotePath+"?from=dev", strings.NewReader(tt.body))
			req.SetPathValue("ch", "staging")
			w := httptest.NewRecorder()
			(&Handlers{}).For(Route{Method: http.MethodPost, Path: promotePath}).ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// TestAuditCursorRoundTrip pins the opaque cursor: base64url of {"before":id}, no
// padding, and a malformed value is a 400.
func TestAuditCursorRoundTrip(t *testing.T) {
	got := encodeAuditCursor(42)
	if strings.Contains(got, "=") {
		t.Errorf("cursor %q contains padding", got)
	}
	body, err := base64.RawURLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("cursor is not base64url: %v", err)
	}
	if string(body) != `{"before":42}` {
		t.Errorf("cursor body = %s, want {\"before\":42}", body)
	}
	id, err := decodeAuditCursor(got)
	if err != nil || id != 42 {
		t.Fatalf("decode = %d, %v, want 42, nil", id, err)
	}
}

func TestAuditCursorRejectsMalformed(t *testing.T) {
	emptyObject := base64.RawURLEncoding.EncodeToString([]byte(`{}`))
	notJSON := base64.RawURLEncoding.EncodeToString([]byte(`before=1`))
	for _, raw := range []string{"!!!not-base64!!!", emptyObject, notJSON, "MTIz"} {
		req := httptest.NewRequest(http.MethodGet, auditPath+"?cursor="+raw, nil)
		w := httptest.NewRecorder()
		(&Handlers{}).For(Route{Method: http.MethodGet, Path: auditPath}).ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("cursor %q = %d, want 400 (%s)", raw, w.Code, w.Body.String())
		}
	}
}

// TestAuditValidation covers the query-parameter rules and the no-store 500.
func TestAuditValidation(t *testing.T) {
	badLimits := []string{"?limit=0", "?limit=201", "?limit=x"}
	for _, q := range badLimits {
		req := httptest.NewRequest(http.MethodGet, auditPath+q, nil)
		w := httptest.NewRecorder()
		(&Handlers{}).For(Route{Method: http.MethodGet, Path: auditPath}).ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", q, w.Code)
		}
	}
	badTimes := []string{"?from=yesterday", "?to=not-a-time"}
	for _, q := range badTimes {
		req := httptest.NewRequest(http.MethodGet, auditPath+q, nil)
		w := httptest.NewRecorder()
		(&Handlers{}).For(Route{Method: http.MethodGet, Path: auditPath}).ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", q, w.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, auditPath, nil)
	w := httptest.NewRecorder()
	(&Handlers{}).For(Route{Method: http.MethodGet, Path: auditPath}).ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("valid audit request without a store = %d, want 500", w.Code)
	}
}
