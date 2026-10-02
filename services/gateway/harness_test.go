package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"gateway/gateway/internal/authn"
	"gateway/gateway/internal/config"
	"gateway/gateway/internal/proxy"
	"gateway/gateway/internal/router"
	"gateway/gateway/internal/server"
)

// Shared harness for the package-main tests (route policy and rate limiting).
// It builds the gateway the way main does: config.Load, proxy.NewRegistry,
// verifyUpstreams, buildPublicMux and server.BuildPublicHandler, with the real
// route table from router.BuildRoutes. Nothing here depends on Auth or PHP
// Admin Auth running: both JWKS endpoints are httptest servers with ephemeral
// Ed25519 keys (identity contract §10).

// Identity values from the identity contract (§9.2), the same ones
// internal/authn/fixture_test.go uses.
const (
	playerIss = "https://auth.otomo.internal"
	playerAud = "otomo:player"
	staffIss  = "https://admin-auth.otomo.internal"
	staffAud  = "otomo:staff"
)

// testJWKS is one ephemeral Ed25519 key published as a JWK Set.
type testJWKS struct {
	srv  *httptest.Server
	priv ed25519.PrivateKey
	kid  string
}

func newTestJWKS(t *testing.T, kid string) *testJWKS {
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
	return &testJWKS{srv: srv, priv: priv, kid: kid}
}

func (j *testJWKS) sign(t *testing.T, claims authn.Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = j.kid
	s, err := tok.SignedString(j.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// playerClaims are valid player-domain claims: no roles (identity contract §4)
// and a subject, which the middleware requires.
func playerClaims() authn.Claims {
	return authn.Claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer:    playerIss,
		Audience:  jwt.ClaimStrings{playerAud},
		Subject:   "player-1",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
	}}
}

func staffClaims(roles ...string) authn.Claims {
	return authn.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    staffIss,
			Audience:  jwt.ClaimStrings{staffAud},
			Subject:   "staff-1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
		Roles: roles,
	}
}

type testGW struct {
	url    string
	client *http.Client
	cfg    *config.Config
	// upstreamHits counts requests that reached the fake upstream. A rejected
	// request must leave it unchanged: fail closed means nothing is proxied.
	upstreamHits *atomic.Int64
	playerTok    string
	staffTok     string // staff token with the admin role
}

// buildTestGW builds the player gateway with its real route table. env
// overrides GATEWAY_* variables before config.Load runs, so every tunable goes
// through production parsing and validation. The general limit is raised by
// default because tests send many requests from one loopback IP; the login
// limit keeps its production default unless a test overrides it.
//
// The harness uses t.Setenv, so tests that call it cannot use t.Parallel.
func buildTestGW(t *testing.T, env map[string]string) *testGW {
	t.Helper()

	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "upstream-ok:%s", r.URL.Path)
	}))
	t.Cleanup(upstream.Close)

	playerJWKS := newTestJWKS(t, "player-key")
	staffJWKS := newTestJWKS(t, "staff-key")

	routes := router.BuildRoutes()
	vars := map[string]string{
		"GATEWAY_LISTEN_ADDR":      "127.0.0.1:0",
		"GATEWAY_METRICS_ADDR":     "127.0.0.1:0",
		"GATEWAY_PLAYER_JWKS_URL":  playerJWKS.srv.URL,
		"GATEWAY_PLAYER_ISSUER":    playerIss,
		"GATEWAY_PLAYER_AUDIENCE":  playerAud,
		"GATEWAY_STAFF_JWKS_URL":   staffJWKS.srv.URL,
		"GATEWAY_STAFF_ISSUER":     staffIss,
		"GATEWAY_STAFF_AUDIENCE":   staffAud,
		"GATEWAY_RATE_LIMIT_RPS":   "1000",
		"GATEWAY_RATE_LIMIT_BURST": "2000",
	}
	for _, r := range routes {
		vars["GATEWAY_UPSTREAM_"+strings.ToUpper(r.Upstream)+"_URL"] = upstream.URL
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

	ts := httptest.NewServer(server.BuildPublicHandler(mux, cfg.TrustedProxies))
	t.Cleanup(ts.Close)
	// A bounded client turns a request that blocks into a named test failure
	// instead of a package-wide timeout.
	client := ts.Client()
	client.Timeout = 10 * time.Second

	return &testGW{
		url:          ts.URL,
		client:       client,
		cfg:          cfg,
		upstreamHits: &hits,
		playerTok:    playerJWKS.sign(t, playerClaims()),
		staffTok:     staffJWKS.sign(t, staffClaims("admin")),
	}
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
