package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/otomo-live/otomo/services/dashboard/internal/auth"
)

// TestRouteTableIsWellFormed checks the table the whole service is built from: no pattern
// is registered twice (ServeMux would panic at start-up on a duplicate, which is worse than
// a test failure), every route is under the service's own prefix so a typo cannot register
// a route Gateway will never forward to, and every route is viewer — the Dashboard is
// read-only, so a higher bar anywhere in this table would lock a viewer out of a panel
// design/01-dashboard.md §4 says they may see.
func TestRouteTableIsWellFormed(t *testing.T) {
	seen := make(map[string]Route)
	for _, rt := range Routes() {
		if rt.Method == "" || rt.Path == "" {
			t.Errorf("incomplete route: %+v", rt)
			continue
		}
		if !strings.HasPrefix(rt.Path, "/api/admin/dashboard/") {
			t.Errorf("%s %s is outside /api/admin/dashboard/", rt.Method, rt.Path)
		}
		if rt.MinRole != auth.RoleViewer {
			t.Errorf("%s %s requires %s, want viewer", rt.Method, rt.Path, rt.MinRole)
		}
		if rt.Method != http.MethodGet {
			t.Errorf("%s %s is not a GET; §4 defines a read-only surface", rt.Method, rt.Path)
		}
		if _, dup := seen[rt.Pattern()]; dup {
			t.Errorf("duplicate route %s", rt.Pattern())
		}
		seen[rt.Pattern()] = rt
	}
	if len(seen) != 6 {
		t.Errorf("the table has %d routes; §4 lists 6", len(seen))
	}
}

// TestNotFoundOrMethodNotAllowed is the behaviour COM-5 asks for that net/http does not
// give on its own: its own 405 is a plain-text body with no envelope.
func TestNotFoundOrMethodNotAllowed(t *testing.T) {
	routes := Routes()
	fallback := NotFoundOrMethodNotAllowed(routes)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
		wantAllow  string
	}{
		{
			name:       "unknown path",
			method:     http.MethodGet,
			path:       "/api/admin/dashboard/nope",
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "wrong method on a real path",
			method:     http.MethodDelete,
			path:       "/api/admin/dashboard/overview",
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
			wantAllow:  "GET, HEAD",
		},
		{
			// The wildcard is resolved by the pattern matcher, which is why the allowed
			// methods come from a probe mux rather than from string equality: {name} has
			// to match "gateway" before the answer means anything.
			name:       "wrong method under a wildcard",
			method:     http.MethodDelete,
			path:       "/api/admin/dashboard/services/gateway/series",
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
			wantAllow:  "GET, HEAD",
		},
		{
			name:       "outside the service's prefix",
			method:     http.MethodGet,
			path:       "/metrics",
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "root",
			method:     http.MethodGet,
			path:       "/",
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, nil)
			r = r.WithContext(WithRequestID(r.Context(), "req-1"))
			w := httptest.NewRecorder()

			fallback(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if got := w.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("Allow = %q, want %q", got, tt.wantAllow)
			}

			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not the COM-5 envelope: %v (%s)", err, w.Body.String())
			}
			if body.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.wantCode)
			}
			if body.Error.Message == "" {
				t.Error("message is empty")
			}
			if body.Error.RequestID != "req-1" {
				t.Errorf("request_id = %q, want req-1", body.Error.RequestID)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

func TestWriteErrorWithoutARequestID(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusForbidden, "insufficient_role", "nope")

	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Error.RequestID != "" {
		t.Errorf("request_id = %q, want empty", body.Error.RequestID)
	}
	if body.Error.Code != "insufficient_role" {
		t.Errorf("code = %q", body.Error.Code)
	}
}

func TestRequestIDRoundTrip(t *testing.T) {
	if RequestID(t.Context()) != "" {
		t.Error("RequestID returned a value for a context that never carried one")
	}
	if got := RequestID(WithRequestID(t.Context(), "abc")); got != "abc" {
		t.Errorf("RequestID = %q, want abc", got)
	}
}

func TestHealthz(t *testing.T) {
	w := httptest.NewRecorder()
	Healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("body = %q", w.Body.String())
	}
}

// TestReadyz reports a 503 while a dependency is unhappy, and names the dependency: a
// readiness probe that only says "not ready" is a probe nobody can act on.
func TestReadyz(t *testing.T) {
	t.Run("no check", func(t *testing.T) {
		w := httptest.NewRecorder()
		Readyz(nil)(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("passing check", func(t *testing.T) {
		w := httptest.NewRecorder()
		Readyz(func(context.Context) error { return nil })(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("failing check", func(t *testing.T) {
		w := httptest.NewRecorder()
		check := func(context.Context) error {
			return errors.New("staff jwks not fetched yet from http://php-admin/.well-known/jwks.json")
		}
		Readyz(check)(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", w.Code)
		}
		if !strings.Contains(w.Body.String(), "staff jwks not fetched") {
			t.Errorf("body does not name the failing dependency: %q", w.Body.String())
		}
	})

	t.Run("check sees the request context", func(t *testing.T) {
		// A check that cannot see the caller's context cannot stop on a client disconnect,
		// which matters for anything with a timeout behind it.
		var got context.Context
		check := func(ctx context.Context) error {
			got = ctx
			return nil
		}
		r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		Readyz(check)(httptest.NewRecorder(), r)
		if got == nil {
			t.Fatal("the check was not called")
		}
		if got != r.Context() {
			t.Error("the check did not receive the request's context")
		}
	})
}

func TestNotImplementedIsAValidHandler(t *testing.T) {
	w := httptest.NewRecorder()
	NotImplemented(w, httptest.NewRequest(http.MethodGet, "/api/admin/dashboard/overview", nil))

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Error.Code != "not_implemented" {
		t.Errorf("code = %q, want not_implemented", body.Error.Code)
	}
}
