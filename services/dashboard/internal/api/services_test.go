package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/health"
)

// servicesRoute is the route whose pattern For must recognise.
func servicesRoute() Route {
	return Route{Method: http.MethodGet, Path: servicesPath, MinRole: 0}
}

func TestHandlersForServices(t *testing.T) {
	readyAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	checkedAt := readyAt.Add(time.Minute)

	statuses := []health.Status{
		{Name: "patch", Up: true, Ready: false, Reason: "boom", LatencyMs: 12, CheckedAt: checkedAt},
		{Name: "auth", Up: true, Ready: true, LatencyMs: 3, CheckedAt: checkedAt, LastReadyAt: &readyAt},
	}
	h := &Handlers{Services: func() []health.Status { return statuses }}

	w := httptest.NewRecorder()
	h.For(servicesRoute()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, servicesPath, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Services []health.Status `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v (%s)", err, w.Body.String())
	}
	if len(body.Services) != 2 {
		t.Fatalf("got %d services, want 2", len(body.Services))
	}
	if body.Services[0].Name != "auth" || body.Services[1].Name != "patch" {
		t.Errorf("services are not sorted by name: %+v", body.Services)
	}
	if !body.Services[0].Ready || body.Services[0].LastReadyAt == nil {
		t.Errorf("auth status = %+v, want ready with last_ready_at", body.Services[0])
	}
	if body.Services[1].Reason != "boom" || body.Services[1].LatencyMs != 12 {
		t.Errorf("patch status = %+v", body.Services[1])
	}
}

func TestHandlersForServicesEmpty(t *testing.T) {
	for _, tt := range []struct {
		name     string
		handlers *Handlers
	}{
		{"no provider", &Handlers{}},
		{"nil provider result", &Handlers{Services: func() []health.Status { return nil }}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.handlers.For(servicesRoute()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, servicesPath, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatalf("body: %v", err)
			}
			if got := string(raw["services"]); got != "[]" {
				t.Errorf("services = %s, want []", got)
			}
		})
	}
}

// TestHandlersForUnknownRoute checks the seam's fail-closed direction: a route the handler
// table does not know still lands on the 501 placeholder.
func TestHandlersForUnknownRoute(t *testing.T) {
	h := &Handlers{Services: func() []health.Status { return nil }}

	w := httptest.NewRecorder()
	h.For(Route{Method: http.MethodGet, Path: "/api/admin/dashboard/nope"}).
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/dashboard/nope", nil))

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}
