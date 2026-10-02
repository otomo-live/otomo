package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"gateway/gateway/internal/router"
)

// TestHandlerFor_DropsCookieHeader asserts that a client Cookie never reaches
// the upstream. The player edge may share a parent domain with other sites, so
// a browser sends us any cookie they set with Domain=<parent> even though the player API is bearer-token only. Both a normal and a Stream route
// are covered because HandlerFor has a separate streaming path.
func TestHandlerFor_DropsCookieHeader(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
	}{
		{name: "normal"},
		{name: "stream", stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotCookie []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotCookie = r.Header.Values("Cookie")
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(upstream.Close)

			upstreamURL, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			reg := NewRegistry(map[string]*url.URL{"session": upstreamURL})
			t.Cleanup(reg.CloseIdleConnections)

			route := router.Route{
				Method: "*", Pattern: "/api/player/session/", Upstream: "session",
				Group: router.GroupPlayer, Stream: tc.stream,
			}
			handler := reg.HandlerFor(route)

			req := httptest.NewRequest(http.MethodGet, "/api/player/session/x", nil)
			req.Header.Set("Cookie", "a=b; __Host-x=y")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			// A 200 proves the request reached the upstream; without this a
			// proxy failure could pass the cookie assertion vacuously.
			if rr.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rr.Code)
			}
			if len(gotCookie) != 0 {
				t.Errorf("upstream received Cookie header %q, want none", gotCookie)
			}
		})
	}
}
