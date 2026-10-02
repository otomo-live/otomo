package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/otomo-live/otomo/services/gateway_dev/internal/router"
)

// upstreamCookies records what an upstream received, including whether the
// Cookie header was present at all: an empty header and an absent one both
// read as "" via Header.Get, and the latter is what the strip must produce.
type upstreamCookies struct {
	header  string
	present bool
	cookies []*http.Cookie
}

// newCookieCaptureServer wires one route through the real proxy to a stub
// upstream and returns the gateway server plus the captured Cookie state.
func newCookieCaptureServer(t *testing.T, route router.Route) (*httptest.Server, *upstreamCookies) {
	t.Helper()

	var got upstreamCookies
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.header = r.Header.Get("Cookie")
		_, got.present = r.Header["Cookie"]
		got.cookies = r.Cookies()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	reg := NewRegistry(map[string]*url.URL{route.Upstream: upstreamURL})
	t.Cleanup(reg.CloseIdleConnections)

	mux := http.NewServeMux()
	mux.Handle(route.MuxPattern(), reg.HandlerFor(route))

	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)
	return gw, &got
}

func cookieValue(cookies []*http.Cookie, name string) (string, bool) {
	for _, c := range cookies {
		if c.Name == name {
			return c.Value, true
		}
	}
	return "", false
}

// TestHandlerFor_AdminAuthKeepsStaffCookie: admin-auth is the one upstream the
// Path=/ refresh credential is for, so a ForwardCookies route must forward it.
func TestHandlerFor_AdminAuthKeepsStaffCookie(t *testing.T) {
	route := router.Route{Method: "GET", Pattern: "/admin-auth/", Upstream: "adminauth", ForwardCookies: true}
	gw, captured := newCookieCaptureServer(t, route)

	req, err := http.NewRequest("GET", gw.URL+"/admin-auth/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "__Host-otomo_refresh", Value: "refresh-token"})

	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /admin-auth/: %v", err)
	}
	resp.Body.Close()

	if v, ok := cookieValue(captured.cookies, "__Host-otomo_refresh"); !ok || v != "refresh-token" {
		t.Errorf("upstream got __Host-otomo_refresh = %q (present %v), want refresh-token", v, ok)
	}
}

// TestHandlerFor_OtherRouteDropsEveryCookie: a route that does not declare
// ForwardCookies passes no cookie at all upstream - neither the refresh
// credential nor anything a sibling site under the shared parent domain set.
func TestHandlerFor_OtherRouteDropsEveryCookie(t *testing.T) {
	route := router.Route{Method: "GET", Pattern: "/api/admin/config/", Upstream: "config"}
	gw, captured := newCookieCaptureServer(t, route)

	req, err := http.NewRequest("GET", gw.URL+"/api/admin/config/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "__Host-otomo_refresh", Value: "refresh-token"})
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})

	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/admin/config/: %v", err)
	}
	resp.Body.Close()

	if v, ok := cookieValue(captured.cookies, "__Host-otomo_refresh"); ok {
		t.Errorf("refresh cookie reached config upstream with value %q", v)
	}
	if captured.present {
		t.Errorf("Cookie header reached config upstream: %q", captured.header)
	}
}

// TestHandlerFor_OnlyStaffCookieDropsCookieHeader: with nothing left to send,
// the Cookie header must be gone, not forwarded empty.
func TestHandlerFor_OnlyStaffCookieDropsCookieHeader(t *testing.T) {
	route := router.Route{Method: "GET", Pattern: "/api/admin/session/", Upstream: "session"}
	gw, captured := newCookieCaptureServer(t, route)

	req, err := http.NewRequest("GET", gw.URL+"/api/admin/session/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "__Host-otomo_refresh", Value: "refresh-token"})

	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/admin/session/: %v", err)
	}
	resp.Body.Close()

	if captured.present {
		t.Errorf("Cookie header still present at upstream: %q", captured.header)
	}
	if captured.header != "" {
		t.Errorf("upstream Cookie header = %q, want empty", captured.header)
	}
}

// TestDevRoutesForwardCookiesOnlyToAdminAuth pins the route table's side of the
// rule: exactly the admin-auth upstream's routes receive cookies.
func TestDevRoutesForwardCookiesOnlyToAdminAuth(t *testing.T) {
	for _, r := range router.BuildRoutes() {
		if r.ForwardCookies != (r.Upstream == "adminauth") {
			t.Errorf("%s %s (upstream %s): ForwardCookies = %v", r.Method, r.Pattern, r.Upstream, r.ForwardCookies)
		}
	}
}
