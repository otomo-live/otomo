package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/gateway_dev/internal/authn"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/config"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/proxy"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/router"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/server"
)

// Route policy for the admin/dev edge, the counterpart of
// services/gateway/route_policy_test.go (GATE-5) for
// internal/router/dev.go's table.
//
// The policy here is the admin half of techspec §9: /admin-auth/* and /admin/*
// are public (how a staff client gets a token and its static bundle), every
// /api/admin/* route is GroupStaff, and no player path is registered at all.
// The tests below check that table against a literal copy of it (so a change
// must edit this file), against the group invariants, and end to end through
// the production wiring with a stub upstream: every staff route × the eight
// token kinds an edge can meet.

// The identity values the test harness mints against. They are the ones the
// identity contract §9.2 and internal/authn/fixture_test.go use.
const (
	policyStaffIssuer    = "https://admin-auth.otomo.internal"
	policyStaffAudience  = "otomo:staff"
	policyPlayerIssuer   = "https://auth.otomo.internal"
	policyPlayerAudience = "otomo:player"
)

// policyJWKS is one ephemeral Ed25519 keypair published as a JWK Set, with the
// private half kept so the test can mint tokens. It is the minimum of
// internal/authn/fixture_test.go's fixture needed outside that package: this
// service's table only ever uses the staff domain, and the player key exists
// here solely to prove a player token is rejected.
type policyJWKS struct {
	srv  *httptest.Server
	kid  string
	priv ed25519.PrivateKey
}

func newPolicyJWKS(t *testing.T, kid string) *policyJWKS {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]any{"keys": []map[string]any{{
		"kty": "OKP", "crv": "Ed25519", "kid": kid,
		"x": base64.RawURLEncoding.EncodeToString(pub), "use": "sig", "alg": "EdDSA",
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(srv.Close)
	return &policyJWKS{srv: srv, kid: kid, priv: priv}
}

func (j *policyJWKS) sign(t *testing.T, claims authn.Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = j.kid
	s, err := tok.SignedString(j.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func policyStaffClaims(roles ...string) authn.Claims {
	return authn.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    policyStaffIssuer,
			Audience:  jwt.ClaimStrings{policyStaffAudience},
			Subject:   "staff-1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
		Roles: roles,
	}
}

func policyExpiredStaffClaims() authn.Claims {
	c := policyStaffClaims("admin")
	c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
	return c
}

func policyPlayerClaims() authn.Claims {
	return authn.Claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer:    policyPlayerIssuer,
		Audience:  jwt.ClaimStrings{policyPlayerAudience},
		Subject:   "player-1",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
	}}
}

// wantDevRoutes is internal/router/dev.go's BuildRoutes table, written out
// literally. It is deliberately not derived from BuildRoutes: a change to the
// table that nobody reflected here is the failure this test exists to catch.
var wantDevRoutes = []router.Route{
	{Method: "*", Pattern: "/admin-auth/", Upstream: "adminauth", Group: router.GroupPublic, SetForwarded: true, ForwardCookies: true},
	{Method: "*", Pattern: "/admin/", Upstream: "adminui", Group: router.GroupPublic},
	{Method: "*", Pattern: "/api/admin/config/", Upstream: "config", Group: router.GroupStaff, MinRole: router.RoleViewer},
	{Method: "POST", Pattern: "/api/admin/config/channels/live/releases", Upstream: "config", Group: router.GroupStaff, MinRole: router.RoleAdmin},
	{Method: "POST", Pattern: "/api/admin/config/packs", Upstream: "config", Group: router.GroupStaff, MinRole: router.RoleLiveOps, MaxBody: 512 << 20, Upload: true},
	{Method: "*", Pattern: "/api/admin/dashboard/logs/tail", Upstream: "dashboard", Group: router.GroupStaff, MinRole: router.RoleViewer, Stream: true},
	{Method: "*", Pattern: "/api/admin/dashboard/", Upstream: "dashboard", Group: router.GroupStaff, MinRole: router.RoleViewer},
	{Method: "*", Pattern: "/api/admin/session/", Upstream: "session", Group: router.GroupStaff, MinRole: router.RoleViewer},
	{Method: "*", Pattern: "/api/admin/users", Upstream: "adminauth", Group: router.GroupStaff, MinRole: router.RoleAdmin, SetForwarded: true, ForwardCookies: true},
	{Method: "*", Pattern: "/api/admin/users/", Upstream: "adminauth", Group: router.GroupStaff, MinRole: router.RoleAdmin, SetForwarded: true, ForwardCookies: true},
}

// Acceptance: the table is exactly the one written down here. Every field the
// route carries (Method, Pattern, Upstream, Group, MinRole, Stream,
// SetForwarded, MaxBody, Upload) is compared, so nothing about a route can
// change without editing this test. StripPrefix is not in the list because no
// route here uses it; if one ever did, the struct comparison would still catch
// it.
func TestRoutePolicy_MatchesSpec(t *testing.T) {
	got := router.BuildRoutes()
	if len(got) != len(wantDevRoutes) {
		t.Fatalf("route table has %d routes, want %d", len(got), len(wantDevRoutes))
	}
	for i, r := range got {
		if r != wantDevRoutes[i] {
			t.Errorf("route %d: got %+v, want %+v", i, r, wantDevRoutes[i])
		}
	}
}

// Acceptance: the table's fields agree with the group policy.
//   - Public routes carry no MinRole: the middleware skips public routes, so a
//     role there would be a fail-open trap.
//   - Staff routes need at least viewer, so any valid staff token clears the
//     coarsest bar; Config re-checks the finer per-route bar.
//   - GroupPlayer must not appear: this binary is the admin edge, and the
//     constant survives only for the cross-domain middleware tests.
func TestRoutePolicy_FieldsConsistentWithGroup(t *testing.T) {
	for _, r := range router.BuildRoutes() {
		switch r.Group {
		case router.GroupPublic:
			if r.MinRole != 0 {
				t.Errorf("public route %s %s has MinRole %d, which is never checked", r.Method, r.Pattern, r.MinRole)
			}
		case router.GroupStaff:
			if r.MinRole < router.RoleViewer {
				t.Errorf("staff route %s %s has MinRole %d, want >= viewer", r.Method, r.Pattern, r.MinRole)
			}
		case router.GroupPlayer:
			t.Errorf("player route %s %s exists in the admin table", r.Method, r.Pattern)
		default:
			t.Errorf("route %s %s has unknown Group %d", r.Method, r.Pattern, r.Group)
		}
	}
}

// policyGW is the admin table wired the way main wires it (config.Load,
// proxy.NewRegistry, verifyUpstreams, buildPublicMux) against a stub upstream
// and an ephemeral staff JWKS, plus the eight tokens the matrix presents.
type policyGW struct {
	url          string
	client       *http.Client
	upstreamHits *atomic.Int64
	tokens       map[string]string
}

func buildPolicyGW(t *testing.T) *policyGW {
	t.Helper()

	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	staff := newPolicyJWKS(t, "staff-key")
	player := newPolicyJWKS(t, "player-key")

	routes := router.BuildRoutes()
	vars := map[string]string{
		"GATEWAY_DEV_LISTEN_ADDR":      "127.0.0.1:0",
		"GATEWAY_DEV_METRICS_ADDR":     "127.0.0.1:0",
		"GATEWAY_STAFF_JWKS_URL":       staff.srv.URL,
		"GATEWAY_STAFF_ISSUER":         policyStaffIssuer,
		"GATEWAY_STAFF_AUDIENCE":       policyStaffAudience,
		"GATEWAY_DEV_RATE_LIMIT_RPS":   "10000",
		"GATEWAY_DEV_RATE_LIMIT_BURST": "100000",
	}
	for _, r := range routes {
		vars["GATEWAY_DEV_UPSTREAM_"+strings.ToUpper(r.Upstream)+"_URL"] = upstream.URL
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	reg := proxy.NewRegistry(cfg.Upstreams)
	if err := verifyUpstreams(routes, reg); err != nil {
		t.Fatalf("verifyUpstreams: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mux, jwks, err := buildPublicMux(ctx, cfg, routes, reg)
	if err != nil {
		t.Fatalf("buildPublicMux: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !jwks.Ready(ctx) {
		if time.Now().After(deadline) {
			t.Fatal("JWKS registry never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ts := httptest.NewServer(server.BuildPublicHandler(mux, nil))
	t.Cleanup(ts.Close)
	client := ts.Client()
	client.Timeout = 10 * time.Second

	return &policyGW{
		url:          ts.URL,
		client:       client,
		upstreamHits: &hits,
		tokens: map[string]string{
			"none":          "",
			"garbage":       "not.a.jwt",
			"expired_staff": staff.sign(t, policyExpiredStaffClaims()),
			"player":        player.sign(t, policyPlayerClaims()),
			"no_roles":      staff.sign(t, policyStaffClaims()),
			"viewer":        staff.sign(t, policyStaffClaims("viewer")),
			"live_ops":      staff.sign(t, policyStaffClaims("live_ops")),
			"admin":         staff.sign(t, policyStaffClaims("admin")),
		},
	}
}

// doProxied sends one request and reports the status, the COM-5 error code (if
// the body is one) and whether it reached the stub upstream.
func (gw *policyGW) doProxied(t *testing.T, method, path, token string) (status int, code string, proxied bool) {
	t.Helper()
	before := gw.upstreamHits.Load()
	req, err := http.NewRequest(method, gw.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := gw.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") == "application/json" {
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		b, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(b, &body); err == nil {
			code = body.Error.Code
		}
	}
	return resp.StatusCode, code, gw.upstreamHits.Load() > before
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

func staffRoutes() []router.Route {
	var out []router.Route
	for _, r := range router.BuildRoutes() {
		if r.Group == router.GroupStaff {
			out = append(out, r)
		}
	}
	return out
}

// tokenKind describes one of the eight credentials an edge can meet.
type tokenKind struct {
	name  string
	token string
	// role is the effective role rank the token carries; 0 for every
	// unusable credential. It mirrors authn's roleRank and is written down
	// here rather than imported because that table is unexported.
	role int
	// usableForRole reports whether this token reaches the role check at all.
	// false means the request must be a 401: the token never verified.
	usableForRole bool
}

// Acceptance (fail closed): every staff route is probed through the
// production wiring with no token, garbage, an expired staff token, a
// player-domain token, a staff token with no roles, and staff viewer/live_ops/
// admin tokens.
//
//   - none, garbage, expired and player tokens are 401 (never 403, never 2xx):
//     the credential does not verify, so no role decision is reached.
//   - a verified staff token with no roles is 403 insufficient_role.
//   - a verified role token is 2xx when its role meets the route's MinRole and
//     403 insufficient_role when it does not.
//
// A rejection must never reach the upstream. The token kinds are written out
// from the identity contract's role hierarchy (viewer < live_ops < admin), not
// derived from router.Role, so a reordering of the constants fails here.
func TestRoutePolicy_FailClosedMatrix(t *testing.T) {
	gw := buildPolicyGW(t)

	kinds := []tokenKind{
		{name: "none", token: gw.tokens["none"]},
		{name: "garbage", token: gw.tokens["garbage"]},
		{name: "expired_staff", token: gw.tokens["expired_staff"]},
		{name: "player", token: gw.tokens["player"]},
		{name: "no_roles", token: gw.tokens["no_roles"], usableForRole: true, role: 0},
		{name: "viewer", token: gw.tokens["viewer"], usableForRole: true, role: 1},
		{name: "live_ops", token: gw.tokens["live_ops"], usableForRole: true, role: 2},
		{name: "admin", token: gw.tokens["admin"], usableForRole: true, role: 3},
	}

	for _, r := range staffRoutes() {
		minRank := int(r.MinRole)
		for _, tk := range kinds {
			name := r.Method + " " + r.Pattern + " " + tk.name
			status, code, proxied := gw.doProxied(t, probeMethod(r), probePath(r), tk.token)

			if !tk.usableForRole {
				if status != http.StatusUnauthorized {
					t.Errorf("%s: status=%d code=%q proxied=%v, want 401 and not proxied",
						name, status, code, proxied)
				}
				if proxied {
					t.Errorf("%s: an unverifiable token reached the upstream", name)
				}
				continue
			}

			if tk.role >= minRank {
				if status != http.StatusOK || !proxied {
					t.Errorf("%s: status=%d code=%q proxied=%v, want 200 and proxied",
						name, status, code, proxied)
				}
				continue
			}
			if status != http.StatusForbidden || code != "insufficient_role" || proxied {
				t.Errorf("%s: status=%d code=%q proxied=%v, want 403 insufficient_role and not proxied",
					name, status, code, proxied)
			}
		}
	}
}

// Acceptance: the player paths the other gateway serves are not registered
// here. Every one is the COM-5 404, with and without a valid admin token, and
// nothing is proxied.
func TestRoutePolicy_PlayerPathsNotServed(t *testing.T) {
	gw := buildPolicyGW(t)
	for _, p := range []string{
		"/api/player/session/me",
		"/auth/anonymous",
		"/patch/v1/live/manifest",
	} {
		for _, tok := range []string{"", gw.tokens["admin"]} {
			status, code, proxied := gw.doProxied(t, http.MethodGet, p, tok)
			if status != http.StatusNotFound || code != "not_found" || proxied {
				t.Errorf("GET %s: status=%d code=%q proxied=%v, want COM-5 404 not proxied",
					p, status, code, proxied)
			}
		}
	}
}

// Acceptance: no route exposes a .well-known path, and a request for the
// JWKS document is the COM-5 404 that never reaches an upstream, even with a
// valid admin token.
func TestRoutePolicy_JWKSEndpointsNeverProxied(t *testing.T) {
	for _, r := range router.BuildRoutes() {
		if strings.Contains(r.Pattern, ".well-known") {
			t.Errorf("route %s %s exposes a .well-known path", r.Method, r.Pattern)
		}
	}
	gw := buildPolicyGW(t)
	for _, p := range []string{"/.well-known/jwks.json", "/.well-known/staff-jwks.json"} {
		for _, tok := range []string{"", gw.tokens["admin"]} {
			status, code, proxied := gw.doProxied(t, http.MethodGet, p, tok)
			if status != http.StatusNotFound || code != "not_found" || proxied {
				t.Errorf("GET %s: status=%d code=%q proxied=%v, want COM-5 404 not proxied",
					p, status, code, proxied)
			}
		}
	}
}
