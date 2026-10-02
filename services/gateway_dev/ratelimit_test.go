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

	"github.com/otomo-live/otomo/services/gateway_dev/internal/config"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/proxy"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/router"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/server"
)

// GATE-3 rate limiting (techspec §6.4), end to end through buildPublicMux.
// §6.4's acceptance: "Exceeding GATEWAY_DEV_LOGIN_RATE_LIMIT_RPS on
// /admin-auth/* -> 429 in the COM-5 shape". The limiter itself (per-IP buckets,
// sweeper) is unit tested in internal/ratelimit.

// newTestJWKS publishes one ephemeral Ed25519 key as a JWK Set so the staff
// JWKS registry can become ready without admin-auth running.
func newTestJWKS(t *testing.T) *httptest.Server {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]any{"keys": []map[string]any{{
		"kty": "OKP", "crv": "Ed25519", "kid": "staff-key",
		"x": base64.RawURLEncoding.EncodeToString(pub), "use": "sig", "alg": "EdDSA",
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type testGW struct {
	url          string
	client       *http.Client
	cfg          *config.Config
	upstreamHits *atomic.Int64
}

// buildTestGW builds the gateway the way main does: config.Load,
// proxy.NewRegistry, verifyUpstreams, buildPublicMux and
// server.BuildPublicHandler, with the real route table from
// router.BuildRoutes. env overrides GATEWAY_DEV_* variables before config.Load
// runs, so every tunable goes through production parsing and validation.
//
// The harness uses t.Setenv, so tests that call it cannot use t.Parallel.
func buildTestGW(t *testing.T, env map[string]string) *testGW {
	t.Helper()

	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	staffJWKS := newTestJWKS(t)

	routes := router.BuildRoutes()
	vars := map[string]string{
		"GATEWAY_DEV_LISTEN_ADDR":  "127.0.0.1:0",
		"GATEWAY_DEV_METRICS_ADDR": "127.0.0.1:0",
		"GATEWAY_STAFF_JWKS_URL":   staffJWKS.URL,
		"GATEWAY_STAFF_ISSUER":     "https://admin-auth.otomo.internal",
		"GATEWAY_STAFF_AUDIENCE":   "otomo:staff",
	}
	for _, r := range routes {
		vars["GATEWAY_DEV_UPSTREAM_"+strings.ToUpper(r.Upstream)+"_URL"] = upstream.URL
	}
	for k, v := range env {
		vars[k] = v
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

	mux, _, err := buildPublicMux(ctx, cfg, routes, reg)
	if err != nil {
		t.Fatalf("buildPublicMux: %v", err)
	}

	ts := httptest.NewServer(server.BuildPublicHandler(mux, nil))
	t.Cleanup(ts.Close)
	client := ts.Client()
	client.Timeout = 10 * time.Second

	return &testGW{url: ts.URL, client: client, cfg: cfg, upstreamHits: &hits}
}

// do sends one request with an optional bearer token.
func (gw *testGW) do(t *testing.T, method, path, token string) *http.Response {
	t.Helper()
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
	return resp
}

// doProxied sends one request and reports the status, the COM-5 error code
// (if the body is one) and whether the request reached the upstream.
func (gw *testGW) doProxied(t *testing.T, method, path, token string) (status int, code string, proxied bool) {
	t.Helper()
	before := gw.upstreamHits.Load()
	resp := gw.do(t, method, path, token)
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

// Every login pattern must be a real route, or a rename would silently drop
// the stricter limit from the brute-force target.
func TestRateLimit_LoginPatternsExistInRouteTable(t *testing.T) {
	patterns := make(map[string]bool)
	for _, r := range router.BuildRoutes() {
		patterns[r.Pattern] = true
	}
	for p := range loginPatterns {
		if !patterns[p] {
			t.Errorf("login pattern %q is not a route in router.BuildRoutes()", p)
		}
	}
}

// Same guard for the unlimited set: a renamed static-bundle route would
// silently pick up the general limiter and start 429ing asset fetches.
func TestRateLimit_UnlimitedPatternsExistInRouteTable(t *testing.T) {
	patterns := make(map[string]bool)
	for _, r := range router.BuildRoutes() {
		patterns[r.Pattern] = true
	}
	for p := range unlimitedPatterns {
		if !patterns[p] {
			t.Errorf("unlimited pattern %q is not a route in router.BuildRoutes()", p)
		}
	}
}

// The 11th login in one burst from one IP is the stricter bucket's 429 in the
// COM-5 shape, and it never reaches the upstream. The first ten are proxied.
func TestRateLimit_LoginRouteUsesStricterLimiter(t *testing.T) {
	gw := buildTestGW(t, map[string]string{
		"GATEWAY_DEV_LOGIN_RATE_LIMIT_RPS":   "1",
		"GATEWAY_DEV_LOGIN_RATE_LIMIT_BURST": "10",
	})

	for i := 1; i <= 10; i++ {
		if status, _, proxied := gw.doProxied(t, http.MethodPost, "/admin-auth/login", ""); status != http.StatusOK || !proxied {
			t.Fatalf("login request %d: status=%d proxied=%v, want 200 proxied", i, status, proxied)
		}
	}
	if status, code, proxied := gw.doProxied(t, http.MethodPost, "/admin-auth/login", ""); status != http.StatusTooManyRequests || code != "rate_limit_exceeded" || proxied {
		t.Errorf("11th login request: status=%d code=%q proxied=%v, want 429 rate_limit_exceeded, not proxied",
			status, code, proxied)
	}
}

// The /admin/ static SPA bundle is unlimited: 60 rapid asset fetches never 429
// even with both buckets set to a burst of one.
func TestRateLimit_AdminBundleIsUnlimited(t *testing.T) {
	gw := buildTestGW(t, map[string]string{
		"GATEWAY_DEV_RATE_LIMIT_RPS":         "1",
		"GATEWAY_DEV_RATE_LIMIT_BURST":       "1",
		"GATEWAY_DEV_LOGIN_RATE_LIMIT_RPS":   "1",
		"GATEWAY_DEV_LOGIN_RATE_LIMIT_BURST": "1",
	})

	for i := 0; i < 60; i++ {
		if status, _, proxied := gw.doProxied(t, http.MethodGet, "/admin/assets/x.js", ""); status != http.StatusOK || !proxied {
			t.Fatalf("asset request %d: status=%d proxied=%v, want 200 proxied", i, status, proxied)
		}
	}
}

// Every other route uses the general bucket: after the burst (40) the next
// request from the same IP is 429, and the limiter runs before auth, so an
// unauthenticated request is what spends the burst.
func TestRateLimit_GeneralBucketAfterBurst(t *testing.T) {
	gw := buildTestGW(t, map[string]string{
		"GATEWAY_DEV_RATE_LIMIT_RPS":   "1",
		"GATEWAY_DEV_RATE_LIMIT_BURST": "40",
	})

	for i := 1; i <= 40; i++ {
		status, _, _ := gw.doProxied(t, http.MethodGet, "/api/admin/session/players", "")
		if status == http.StatusTooManyRequests {
			t.Fatalf("request %d: 429 before the burst was spent", i)
		}
	}
	status, code, proxied := gw.doProxied(t, http.MethodGet, "/api/admin/session/players", "")
	if status != http.StatusTooManyRequests || code != "rate_limit_exceeded" || proxied {
		t.Errorf("request 41: status=%d code=%q proxied=%v, want 429 rate_limit_exceeded, not proxied",
			status, code, proxied)
	}
}

// An unknown path is never rate limited: the catch-all 404 stays unlimited
// whatever the configured buckets, so it cannot be used to map the surface.
func TestRateLimit_UnknownPathNeverLimited(t *testing.T) {
	gw := buildTestGW(t, map[string]string{
		"GATEWAY_DEV_RATE_LIMIT_RPS":         "1",
		"GATEWAY_DEV_RATE_LIMIT_BURST":       "1",
		"GATEWAY_DEV_LOGIN_RATE_LIMIT_RPS":   "1",
		"GATEWAY_DEV_LOGIN_RATE_LIMIT_BURST": "1",
	})

	for i := 0; i < 50; i++ {
		status, code, proxied := gw.doProxied(t, http.MethodGet, "/no/such/path", "")
		if status != http.StatusNotFound || code != "not_found" || proxied {
			t.Fatalf("unknown path request %d: status=%d code=%q proxied=%v, want COM-5 404 not proxied",
				i, status, code, proxied)
		}
	}
}
