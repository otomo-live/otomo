package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/otomo-live/otomo/services/session/internal/auth"
)

// specRoutes is design/04-session-minimal.md §5 transcribed by hand, in the document's
// order. It is the second copy on purpose: the test below compares it against Routes(),
// so adding a route to the service without the spec — or the reverse — fails here rather
// than in production. The role is the *minimum* role; player routes carry RoleNone, which
// is what makes the table readable as one document.
var specRoutes = []Route{
	{http.MethodPost, "/api/player/session/me/init", GroupPlayer, auth.RoleNone},
	{http.MethodGet, "/api/player/session/me", GroupPlayer, auth.RoleNone},
	{http.MethodPatch, "/api/player/session/me", GroupPlayer, auth.RoleNone},

	{http.MethodPost, "/api/player/session/presence/heartbeat", GroupPlayer, auth.RoleNone},

	{http.MethodGet, "/api/player/session/friends", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/friends/requests", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/friends/requests/{player_id}/accept", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/friends/requests/{player_id}/decline", GroupPlayer, auth.RoleNone},
	{http.MethodDelete, "/api/player/session/friends/{player_id}", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/blocks/{player_id}", GroupPlayer, auth.RoleNone},
	{http.MethodDelete, "/api/player/session/blocks/{player_id}", GroupPlayer, auth.RoleNone},

	{http.MethodPost, "/api/player/session/party", GroupPlayer, auth.RoleNone},
	{http.MethodGet, "/api/player/session/party", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/party/invites", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/party/invites/{invite_id}/accept", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/party/invites/{invite_id}/decline", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/party/leave", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/party/kick/{player_id}", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/party/promote/{player_id}", GroupPlayer, auth.RoleNone},

	// The lobby (design/14-launch-handoff.md §2.1, LB-2).
	{http.MethodPatch, "/api/player/session/party/settings", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/party/ready", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/party/launch", GroupPlayer, auth.RoleNone},
	{http.MethodPost, "/api/player/session/party/launch/ticket", GroupPlayer, auth.RoleNone},

	{http.MethodGet, "/api/player/session/events", GroupPlayer, auth.RoleNone},

	{http.MethodGet, "/api/admin/session/players", GroupStaff, auth.RoleViewer},
	{http.MethodGet, "/api/admin/session/players/{id}", GroupStaff, auth.RoleViewer},
	{http.MethodPost, "/api/admin/session/parties/{party_id}/disband", GroupStaff, auth.RoleLiveOps},
	{http.MethodGet, "/api/admin/session/audit", GroupStaff, auth.RoleViewer},
}

// TestRoutesMatchTheSpec is the contract test for this package: the route table is the
// service's external shape, and §5 is what it was built from.
func TestRoutesMatchTheSpec(t *testing.T) {
	got := Routes()

	if len(got) != len(specRoutes) {
		t.Errorf("Routes() has %d entries, the spec has %d", len(got), len(specRoutes))
	}

	key := func(r Route) string { return r.Method + " " + r.Path }
	byKey := make(map[string]Route, len(got))
	for _, r := range got {
		if _, dup := byKey[key(r)]; dup {
			t.Errorf("Routes() registers %s twice; the second registration would win in the mux", key(r))
		}
		byKey[key(r)] = r
	}

	for _, want := range specRoutes {
		r, ok := byKey[key(want)]
		if !ok {
			t.Errorf("Routes() is missing %s", key(want))
			continue
		}
		if r.Group != want.Group {
			t.Errorf("%s: group = %s, want %s", key(want), r.Group, want.Group)
		}
		if r.MinRole != want.MinRole {
			t.Errorf("%s: min role = %s, want %s", key(want), r.MinRole, want.MinRole)
		}
	}
}

// TestRoutesHaveNoUnlistedEntries guards the other direction explicitly, so a route added
// to the table without being added to specRoutes above is still caught. Without this, the
// missing-entry check would only ever fail on a removal.
func TestRoutesHaveNoUnlistedEntries(t *testing.T) {
	listed := make(map[string]bool, len(specRoutes))
	for _, r := range specRoutes {
		listed[r.Method+" "+r.Path] = true
	}
	for _, r := range Routes() {
		if !listed[r.Method+" "+r.Path] {
			t.Errorf("Routes() has %s %s, which the spec table here does not list", r.Method, r.Path)
		}
	}
}

// TestEveryRouteIsUnderItsOwnPrefix binds the two things that must agree: a route's
// prefix and its identity group. Gateway routes by prefix, so a staff route registered
// under /api/player would be reachable with a player token in Gateway's eyes and refused
// here — a mismatch that only shows up as a 401 nobody can explain.
func TestEveryRouteIsUnderItsOwnPrefix(t *testing.T) {
	for _, r := range Routes() {
		var want string
		switch r.Group {
		case GroupPlayer:
			want = "/api/player/session/"
		case GroupStaff:
			want = "/api/admin/session/"
		default:
			t.Errorf("%s: unknown group %d", r.Pattern(), r.Group)
			continue
		}
		// No route is registered at the bare prefix today; if one is added, it becomes
		// the prefix itself and the check below is the one to relax.
		if len(r.Path) <= len(want) || r.Path[:len(want)] != want {
			t.Errorf("%s: group %s must be under %s", r.Pattern(), r.Group, want)
		}
	}
}

// TestPlayerRoutesCarryNoRole pins the asymmetry between the groups. A player route with
// a role would be checked against the roles of a player token, and player tokens never
// carry roles — so the route would refuse every player, which is a bug that looks like a
// permissions problem.
func TestPlayerRoutesCarryNoRole(t *testing.T) {
	for _, r := range Routes() {
		if r.Group == GroupPlayer && r.MinRole != auth.RoleNone {
			t.Errorf("%s: a player route carries min role %s", r.Pattern(), r.MinRole)
		}
	}
}

// TestStaffRoutesRequireARole is the reverse: every staff route names a role, because
// RoleNone on a staff route would admit any token from the staff domain — including one
// whose roles claim is empty, which is what a token minted before a role was granted
// looks like.
func TestStaffRoutesRequireARole(t *testing.T) {
	for _, r := range Routes() {
		if r.Group == GroupStaff && r.MinRole == auth.RoleNone {
			t.Errorf("%s: a staff route requires no role", r.Pattern())
		}
	}
}

// TestRoutePatternsAreRegistrable catches a malformed pattern at test time rather than at
// boot: http.ServeMux panics on a bad pattern, so a typo in the table would take the
// process down on start-up rather than fail a request.
func TestRoutePatternsAreRegistrable(t *testing.T) {
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("registering the route table panicked: %v", v)
		}
	}()

	mux := http.NewServeMux()
	for _, r := range Routes() {
		mux.Handle(r.Pattern(), http.NotFoundHandler())
	}
}

// TestRoutesReturnsAFreshSlice keeps the table immutable from outside: the server calls
// Routes once per listener, and a caller that sorted or truncated the slice it received
// would otherwise change what the next caller sees.
func TestRoutesReturnsAFreshSlice(t *testing.T) {
	first := Routes()
	if len(first) == 0 {
		t.Fatal("Routes() is empty")
	}
	mutated := first[0]
	mutated.Path = "/mutated"
	first[0] = mutated

	if Routes()[0].Path == "/mutated" {
		t.Error("Routes() shares its backing array between calls")
	}
}

func TestGroupString(t *testing.T) {
	for _, tt := range []struct {
		group Group
		want  string
	}{
		{GroupPlayer, "player"},
		{GroupStaff, "staff"},
		{Group(99), "unknown"},
	} {
		if got := tt.group.String(); got != tt.want {
			t.Errorf("Group(%d).String() = %q, want %q", tt.group, got, tt.want)
		}
	}
}

// fallbackMux builds the public mux the way server.publicHandler does, minus the token
// guard — which is what makes these tests about the fallback handler itself rather than
// about authentication.
func fallbackMux(routes []Route) *http.ServeMux {
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(r.Pattern(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}
	mux.Handle("/", NotFoundOrMethodNotAllowed(routes))
	return mux
}

func TestFallbackNamesTheAllowedMethods(t *testing.T) {
	mux := fallbackMux(Routes())

	tests := []struct {
		name   string
		method string
		path   string
		allow  string
	}{
		{
			// HEAD is listed because a GET pattern matches HEAD too, which is net/http's
			// rule rather than this package's.
			name:   "a path with two methods",
			method: http.MethodDelete,
			path:   "/api/player/session/me",
			allow:  "GET, HEAD, PATCH",
		},
		{
			name:   "a path with one method",
			method: http.MethodPost,
			path:   "/api/player/session/events",
			allow:  "GET, HEAD",
		},
		{
			// The one that needs pattern matching rather than string comparison: the
			// caller has to fill {player_id} for the probe to resolve the route.
			name:   "a path with a wildcard",
			method: http.MethodGet,
			path:   "/api/player/session/friends/requests/018f4a3e-1c2d-7abc-8def-0123456789ab/accept",
			allow:  http.MethodPost,
		},
		{
			// A wildcard under the staff prefix, with a method the path does not take —
			// the row above covers the player prefix, and this one proves the probe is
			// not hard-wired to either.
			name:   "a staff path",
			method: http.MethodDelete,
			path:   "/api/admin/session/players/018f4a3e-1c2d-7abc-8def-0123456789ab",
			allow:  "GET, HEAD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if body := rec.Body.String(); !strings.Contains(body, `"code":"method_not_allowed"`) {
				t.Errorf("body = %s, want the COM-5 envelope", body)
			}
		})
	}
}

// TestFallbackDoesNotAdvertiseUnknownPaths is the enumeration guard: a path with no route
// gets a plain 404 and, crucially, no Allow header — a header naming methods for a path
// that does not exist would confirm which paths do.
func TestFallbackDoesNotAdvertiseUnknownPaths(t *testing.T) {
	mux := fallbackMux(Routes())

	for _, path := range []string{
		"/nope",
		"/api/player/session/nope",
		"/api/admin/session/nope",
		"/metrics",
		"/debug/pprof/",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != "" {
				t.Errorf("Allow = %q, want no header on a 404", allow)
			}
			if body := rec.Body.String(); !strings.Contains(body, `"code":"not_found"`) {
				t.Errorf("body = %s, want the COM-5 envelope", body)
			}
		})
	}
}

// TestFallbackIgnoresAnUnrelatedMethodPath keeps the probe honest about near-misses: a
// path that differs from a real one by a segment must not be treated as the real one, or
// the Allow header would leak that a similarly-spelled route exists.
func TestFallbackIgnoresAnUnrelatedMethodPath(t *testing.T) {
	mux := fallbackMux(Routes())

	rec := httptest.NewRecorder()
	// One segment short of /api/player/session/friends/requests/{id}/accept.
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/player/session/friends/requests", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 for the POST-only path that does exist", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/player/session/friends/requests/1", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for the path that does not", rec.Code)
	}
}
