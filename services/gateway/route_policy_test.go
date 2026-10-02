package main

import (
	"net/http"
	"strings"
	"testing"

	"gateway/gateway/internal/router"
)

// GATE-5: route policy, public vs authenticated.
//
// Techspec §9 says GATE-5 adds no mechanism beyond GATE-2/3; its job is to
// make the route table correct and keep it that way. These tests encode the §9
// policy as literals copied from the spec and check this service's table
// against it, statically and end to end through the production wiring. The
// admin and dev table is services/gateway_dev and is guarded there.
//
// internal/authn/middleware_test.go already holds TestNoPublicRouteUnderAPI
// (§9's second acceptance bullet) and TestUsedGroupsMatchesTheTable; these
// tests add the full policy match and the end-to-end matrix.

// spec9Policy is techspec §9's "explicit policy" list, transcribed as path
// prefixes. It is written out here rather than derived from the route table,
// so a table change that contradicts the spec fails. The admin prefixes stay in
// the list because the policy is per route, not per service: if one of them
// ever appeared in this table it would still have to carry the right group.
var spec9Policy = []struct {
	prefix string
	group  router.Group
}{
	// Public: how you get a token, the admin static bundle, live patch content.
	{"/auth/", router.GroupPublic},
	{"/admin-auth/", router.GroupPublic},
	{"/admin/", router.GroupPublic},
	{"/patch/v1/live/", router.GroupPublic},
	{"/patch/v1/blob/", router.GroupPublic},
	// Player.
	{"/api/player/session/", router.GroupPlayer},
	// Staff.
	{"/api/admin/", router.GroupStaff},
	{"/patch/v1/dev/", router.GroupStaff},
	{"/patch/v1/staging/", router.GroupStaff},
}

// GATE-5 AC (§9 explicit policy): every route falls under exactly one §9
// prefix and carries that prefix's Group. A route with no matching prefix
// fails too, so adding a route forces a policy decision rather than
// inheriting a default.
func TestRoutePolicy_MatchesSpec9(t *testing.T) {
	for _, r := range router.BuildRoutes() {
		var matches []int
		for i, p := range spec9Policy {
			if strings.HasPrefix(r.Pattern, p.prefix) {
				matches = append(matches, i)
			}
		}
		switch len(matches) {
		case 0:
			t.Errorf("route %s %s is not covered by any techspec §9 policy prefix", r.Method, r.Pattern)
		case 1:
			want := spec9Policy[matches[0]]
			if r.Group != want.group {
				t.Errorf("route %s %s has Group %s, §9 policy for %q is %s",
					r.Method, r.Pattern, r.Group, want.prefix, want.group)
			}
		default:
			t.Errorf("route %s %s matches %d §9 policy prefixes; the policy table is ambiguous",
				r.Method, r.Pattern, len(matches))
		}
	}
}

// GATE-5 guard: route fields the auth middleware would silently ignore or
// that contradict the identity contract.
//   - A public route with MinRole set is a fail-open trap: the middleware skips
//     public routes entirely, so the role would never be checked.
//   - Player tokens carry no roles claim (identity contract §4), so a player
//     route with MinRole could never be satisfied.
func TestRoutePolicy_FieldsConsistentWithGroup(t *testing.T) {
	for _, r := range router.BuildRoutes() {
		switch r.Group {
		case router.GroupPublic:
			if r.MinRole != 0 {
				t.Errorf("public route %s %s has MinRole %d, which is never checked", r.Method, r.Pattern, r.MinRole)
			}
		case router.GroupPlayer:
			if r.MinRole != 0 {
				t.Errorf("player route %s %s has MinRole %d, but player tokens carry no roles", r.Method, r.Pattern, r.MinRole)
			}
		case router.GroupStaff:
		default:
			t.Errorf("route %s %s has unknown Group %d", r.Method, r.Pattern, r.Group)
		}
	}
}

// probePath returns a concrete request path the route's pattern matches: the
// pattern itself for exact paths, or one level below for subtree patterns.
func probePath(r router.Route) string {
	if strings.HasSuffix(r.Pattern, "/") {
		return r.Pattern + "probe"
	}
	return r.Pattern
}

func probeMethod(r router.Route) string {
	if r.Method == "*" {
		return http.MethodGet
	}
	return r.Method
}

// GATE-5 AC (§11 definition of done, fail closed): every route in the table
// is probed with no token, a player token and a staff token through the
// production wiring. Public routes pass all three; player routes accept only
// the player token; staff routes accept only the staff token. Every rejection
// is a 401 in the COM-5 shape and never reaches the upstream. Enumerating the
// table means a future route is covered without editing this test.
func TestRoutePolicy_FailClosedMatrix(t *testing.T) {
	gw := buildTestGW(t, nil)

	tokens := []struct {
		name, token string
		domain      router.Group // GroupPublic stands for "no token"
	}{
		{"no_token", "", router.GroupPublic},
		{"player_token", gw.playerTok, router.GroupPlayer},
		{"staff_token", gw.staffTok, router.GroupStaff},
	}

	for _, r := range router.BuildRoutes() {
		for _, tk := range tokens {
			name := r.Method + " " + r.Pattern + " " + tk.name
			status, code, proxied := gw.doProxied(t, probeMethod(r), probePath(r), tk.token)

			if r.Group == router.GroupPublic || r.Group == tk.domain {
				if status != http.StatusOK || !proxied {
					t.Errorf("%s: status=%d proxied=%v, want 200 and proxied", name, status, proxied)
				}
				continue
			}
			// A token from the other domain is signed by a key that domain's
			// JWKS does not hold, so the signature check is what fails first.
			wantCode := "invalid_signature"
			if tk.token == "" {
				wantCode = "missing_token"
			}
			if status != http.StatusUnauthorized || code != wantCode || proxied {
				t.Errorf("%s: status=%d code=%q proxied=%v, want 401 %q and not proxied",
					name, status, code, proxied, wantCode)
			}
		}
	}
}

// GATE-5 AC (§11): "A player token cannot reach any /api/admin/* or
// /patch/v1/{dev,staging}/* route; a staff token cannot reach /api/player/*".
// The matrix covers registered routes; this also probes paths under those
// prefixes that no route registers, so a gap in the table cannot become a
// pass-through. Nothing may be proxied.
func TestRoutePolicy_CrossDomainPrefixesNeverReachable(t *testing.T) {
	gw := buildTestGW(t, nil)

	playerForbidden := []string{
		"/api/admin/config/x", "/api/admin/dashboard/x", "/api/admin/session/x", "/api/admin/unrouted",
		"/patch/v1/dev/manifest", "/patch/v1/dev/blob/x", "/patch/v1/staging/manifest", "/patch/v1/staging/blob/x",
	}
	staffForbidden := []string{
		"/api/player/session/x", "/api/player/session/events", "/api/player/unrouted", "/api/player/match",
	}
	for _, p := range playerForbidden {
		if status, _, proxied := gw.doProxied(t, http.MethodGet, p, gw.playerTok); status < 400 || proxied {
			t.Errorf("player token GET %s: status=%d proxied=%v, want 4xx and not proxied", p, status, proxied)
		}
	}
	for _, p := range staffForbidden {
		if status, _, proxied := gw.doProxied(t, http.MethodGet, p, gw.staffTok); status < 400 || proxied {
			t.Errorf("staff token GET %s: status=%d proxied=%v, want 4xx and not proxied", p, status, proxied)
		}
	}
}

// Gateway split: the admin table lives in services/gateway_dev, so on this
// service every admin path is the COM-5 404, even with a staff admin token.
func TestRoutePolicy_AdminPathsNotServed(t *testing.T) {
	gw := buildTestGW(t, nil)
	for _, p := range []string{
		"/admin-auth/login", "/admin/index.html",
		"/api/admin/config/settings", "/api/admin/config/channels/live/releases",
		"/api/admin/dashboard/overview", "/api/admin/session/x",
	} {
		for _, tok := range []string{"", gw.staffTok} {
			status, code, proxied := gw.doProxied(t, http.MethodGet, p, tok)
			if status != http.StatusNotFound || code != "not_found" || proxied {
				t.Errorf("GET %s: status=%d code=%q proxied=%v, want COM-5 404 not proxied", p, status, code, proxied)
			}
		}
	}
}

// GATE-5 AC (§9): "Auth's and PHP's own JWKS endpoints are never proxied
// through Gateway at all." No route pattern mentions the path, and a request
// for it is a COM-5 404 that never reaches an upstream, even with a valid
// token.
func TestRoutePolicy_JWKSEndpointsNeverProxied(t *testing.T) {
	for _, r := range router.BuildRoutes() {
		if strings.Contains(r.Pattern, ".well-known") {
			t.Errorf("route %s %s exposes a .well-known path", r.Method, r.Pattern)
		}
	}
	gw := buildTestGW(t, nil)
	for _, p := range []string{"/.well-known/jwks.json", "/.well-known/staff-jwks.json", "/auth/../.well-known/jwks.json"} {
		for _, tok := range []string{"", gw.playerTok, gw.staffTok} {
			// ServeMux cleans "/auth/../x" with a redirect to "/x"; the client
			// follows it, so the final answer is still the 404.
			status, code, proxied := gw.doProxied(t, http.MethodGet, p, tok)
			if status != http.StatusNotFound || code != "not_found" || proxied {
				t.Errorf("GET %s: status=%d code=%q proxied=%v, want COM-5 404 not proxied", p, status, code, proxied)
			}
		}
	}
}
