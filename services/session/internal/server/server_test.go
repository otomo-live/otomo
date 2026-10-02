package server

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/session/internal/api"
	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/config"
)

// The two domains as this service is deployed. Repeated here rather than imported from
// the auth package's tests, which are a different package and deliberately do not export
// their fixtures.
const (
	playerIssuer   = "https://auth.otomo.internal"
	playerAudience = "otomo:player"
	staffIssuer    = "https://php-admin.otomo.internal"
	staffAudience  = "otomo:staff"

	// playerID is a valid player subject: a UUID, because that is player_profile's key.
	playerID = "018f4a3e-1c2d-7abc-8def-0123456789ab"
	// Somebody else's player, for the paths that take one.
	otherID = "018f4a3e-1c2d-7abc-8def-0123456789ac"
)

// jwksServer serves a one-key JWKS for pub, which is the only key its verifier will
// accept.
func jwksServer(t *testing.T, pub ed25519.PublicKey) *httptest.Server {
	t.Helper()

	body, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig",
		"kid": "test-key",
		"x":   base64.RawURLEncoding.EncodeToString(pub),
	}}})
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// harness is a running server plus the keys both domains sign with.
type harness struct {
	*Server
	playerKey ed25519.PrivateKey
	staffKey  ed25519.PrivateKey

	publicURL  string
	metricsURL string
	client     *http.Client
}

// newHarness starts a server with both verifiers wired to fresh keys, on ephemeral ports.
// dep overrides are applied last, so a test can replace the readiness check or one of the
// verifiers.
func newHarness(t *testing.T, dep func(*Deps)) *harness {
	t.Helper()

	playerPub, playerKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate player key: %v", err)
	}
	staffPub, staffKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate staff key: %v", err)
	}

	deps := Deps{
		PlayerVerifier: auth.NewPlayerVerifier(jwksServer(t, playerPub).URL, playerIssuer, playerAudience, time.Minute, 0),
		StaffVerifier:  auth.NewStaffVerifier(jwksServer(t, staffPub).URL, staffIssuer, staffAudience, time.Minute, 0),
		Version:        "test",
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	deps.PlayerVerifier.Start(t.Context())
	deps.StaffVerifier.Start(t.Context())
	if dep != nil {
		dep(&deps)
	}

	cfg := config.Config{
		ListenAddr:      "127.0.0.1:0",
		MetricsAddr:     "127.0.0.1:0",
		InternalAddr:    "127.0.0.1:0",
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    10 * time.Second,
		IdleTimeout:     30 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	}
	s := New(cfg, deps)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Run did not return after its context was cancelled")
		}
	})

	select {
	case <-s.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("the listeners never started")
	}

	// The verifiers' first fetch is asynchronous, so a request sent now could be refused
	// for having no keys yet rather than for the token. Wait for each domain to be ready,
	// which is the same thing /readyz gates on. A nil verifier is one the test removed on
	// purpose, and there is nothing to wait for on a domain that answers 500 by design.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if (deps.PlayerVerifier == nil || deps.PlayerVerifier.Ready(t.Context())) &&
			(deps.StaffVerifier == nil || deps.StaffVerifier.Ready(t.Context())) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a verifier never fetched its jwks")
		}
		time.Sleep(5 * time.Millisecond)
	}

	return &harness{
		Server:     s,
		playerKey:  playerKey,
		staffKey:   staffKey,
		publicURL:  "http://" + s.PublicAddr().String(),
		metricsURL: "http://" + s.MetricsAddr().String(),
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// token mints a token for one domain, optionally mutating its claims first.
func (h *harness) token(t *testing.T, domain auth.Domain, mut func(jwt.MapClaims)) string {
	t.Helper()

	now := time.Now()
	c := jwt.MapClaims{"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	var key ed25519.PrivateKey
	if domain == auth.DomainPlayer {
		c["iss"], c["aud"], c["sub"] = playerIssuer, playerAudience, playerID
		key = h.playerKey
	} else {
		c["iss"], c["aud"], c["sub"] = staffIssuer, staffAudience, "staff-1"
		c["roles"] = []string{"viewer"}
		key = h.staffKey
	}
	if mut != nil {
		mut(c)
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	tok.Header["kid"] = "test-key"
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// playerToken is the common case: a valid token for the player domain with a role of its
// own, which the player verifier must ignore.
func (h *harness) playerToken(t *testing.T) string { return h.token(t, auth.DomainPlayer, nil) }

// staffToken mints a staff token carrying exactly roles.
func (h *harness) staffToken(t *testing.T, roles ...string) string {
	return h.token(t, auth.DomainStaff, func(c jwt.MapClaims) {
		if len(roles) == 0 {
			delete(c, "roles")
			return
		}
		c["roles"] = roles
	})
}

// request sends one request to the public listener. An empty token sends no Authorization
// header at all, which is the case a client hits before it has logged in.
func (h *harness) request(t *testing.T, method, path, token string) (int, http.Header, string) {
	t.Helper()

	req, err := http.NewRequest(method, h.publicURL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// scraper hits the internal listener. It is a separate helper because a test that scrapes
// metrics and one that makes a request are asking different questions.
func (h *harness) scrape(t *testing.T, path string) (int, string) {
	t.Helper()

	resp, err := h.client.Get(h.metricsURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// errorCode pulls the COM-5 code out of a response body, failing the test when the body is
// not the envelope — which is itself the assertion that every error is shaped alike.
func errorCode(t *testing.T, body string) string {
	t.Helper()

	var env struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("body is not the COM-5 envelope (%v): %s", err, body)
	}
	if env.Error.Code == "" {
		t.Fatalf("COM-5 envelope carries no code: %s", body)
	}
	return env.Error.Code
}

// TestEveryRouteRefusesAMissingToken is the floor under the whole service: no route in
// §5 is reachable without a token, including the ones that are only stubs. A route that
// answered 501 without a token would be just as wrong as one that answered 200, because
// it would tell an unauthenticated caller which routes exist.
func TestEveryRouteRefusesAMissingToken(t *testing.T) {
	h := newHarness(t, nil)

	for _, rt := range api.Routes() {
		t.Run(rt.Pattern(), func(t *testing.T) {
			// {player_id} and friends have to be filled in for the mux to match; the
			// value is a well-formed UUID so a 400 from a handler could never be
			// mistaken for the 401 under test.
			path := strings.NewReplacer(
				"{player_id}", otherID,
				"{invite_id}", "1",
				"{party_id}", "1",
				"{id}", otherID,
			).Replace(rt.Path)

			status, _, body := h.request(t, rt.Method, path, "")
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
			if code := errorCode(t, body); code != auth.ReasonMissingToken {
				t.Errorf("code = %q, want %q", code, auth.ReasonMissingToken)
			}
		})
	}
}

// TestBadTokensAreRejectedWithTheirReason runs the whole table through the HTTP layer, so
// the reason an operator reads in a response body and the one they read in
// session_token_rejected_total are checked to be the same string the verifier decided on.
func TestBadTokensAreRejectedWithTheirReason(t *testing.T) {
	h := newHarness(t, nil)
	path := "/api/player/session/me"

	tests := []struct {
		name  string
		token func(t *testing.T) string
		code  string
		// status is the expected HTTP status; every case but the forged-signature one is
		// a 401.
		status int
	}{
		{
			name:   "not a jwt",
			token:  func(*testing.T) string { return "not-a-token" },
			code:   auth.ReasonInvalidToken,
			status: http.StatusUnauthorized,
		},
		{
			name: "expired",
			token: func(t *testing.T) string {
				return h.token(t, auth.DomainPlayer, func(c jwt.MapClaims) {
					c["exp"] = time.Now().Add(-2 * time.Hour).Unix()
				})
			},
			code:   auth.ReasonExpired,
			status: http.StatusUnauthorized,
		},
		{
			name: "wrong audience",
			token: func(t *testing.T) string {
				return h.token(t, auth.DomainPlayer, func(c jwt.MapClaims) { c["aud"] = staffAudience })
			},
			code:   auth.ReasonAudienceMismatch,
			status: http.StatusUnauthorized,
		},
		{
			name: "wrong issuer",
			token: func(t *testing.T) string {
				return h.token(t, auth.DomainPlayer, func(c jwt.MapClaims) { c["iss"] = staffIssuer })
			},
			code:   auth.ReasonIssuerMismatch,
			status: http.StatusUnauthorized,
		},
		{
			name: "subject is not a uuid",
			token: func(t *testing.T) string {
				return h.token(t, auth.DomainPlayer, func(c jwt.MapClaims) { c["sub"] = "player-1" })
			},
			code:   auth.ReasonInvalidToken,
			status: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _, body := h.request(t, http.MethodGet, path, tt.token(t))
			if status != tt.status {
				t.Fatalf("status = %d, want %d", status, tt.status)
			}
			if code := errorCode(t, body); code != tt.code {
				t.Errorf("code = %q, want %q", code, tt.code)
			}
		})
	}
}

// TestCrossDomainTokensAreRefused is SES-A1's acceptance criterion at the boundary that
// matters: the HTTP surface. A player token must not open a staff route and a staff token
// must not open a player route, whatever their claims say.
//
// The reason is invalid_signature rather than iss_mismatch, because the two domains
// publish to two key sets: the other domain's kid is not in this key set, so the token is
// unverifiable here before any claim is looked at.
func TestCrossDomainTokensAreRefused(t *testing.T) {
	h := newHarness(t, nil)

	t.Run("a staff token on a player route", func(t *testing.T) {
		status, _, body := h.request(t, http.MethodGet, "/api/player/session/me", h.staffToken(t, "admin"))
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
		if code := errorCode(t, body); code != auth.ReasonInvalidSignature {
			t.Errorf("code = %q, want %q", code, auth.ReasonInvalidSignature)
		}
	})

	t.Run("a player token on a staff route", func(t *testing.T) {
		status, _, body := h.request(t, http.MethodGet, "/api/admin/session/players", h.playerToken(t))
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
		if code := errorCode(t, body); code != auth.ReasonInvalidSignature {
			t.Errorf("code = %q, want %q", code, auth.ReasonInvalidSignature)
		}
	})

	t.Run("a player token carrying admin is still a player token", func(t *testing.T) {
		// Signed by the player key, so it verifies; the roles claim is simply not read on
		// this domain. This is the case the two-issuer design exists for.
		tok := h.token(t, auth.DomainPlayer, func(c jwt.MapClaims) {
			c["roles"] = []string{"admin", "live_ops"}
		})
		status, _, body := h.request(t, http.MethodGet, "/api/admin/session/audit", tok)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
		if code := errorCode(t, body); code != auth.ReasonInvalidSignature {
			t.Errorf("code = %q, want %q", code, auth.ReasonInvalidSignature)
		}
	})
}

// TestStaffRoleIsEnforced walks the role ladder on the staff surface. It is the one place
// where a 403 has to be distinguishable from a 401: the token is good, the caller simply
// may not do this, and telling them "your token is invalid" would send them to re-login.
func TestStaffRoleIsEnforced(t *testing.T) {
	h := newHarness(t, nil)

	tests := []struct {
		name   string
		method string
		path   string
		roles  []string
		status int
		code   string
	}{
		{
			name: "a staff token with no roles at all", method: http.MethodGet,
			path: "/api/admin/session/players", roles: nil,
			status: http.StatusForbidden, code: auth.ReasonInsufficientRole,
		},
		{
			name: "viewer on a read route", method: http.MethodGet,
			path: "/api/admin/session/players", roles: []string{"viewer"},
			status: http.StatusNotImplemented, code: "not_implemented",
		},
		{
			name: "viewer on the force-disband route", method: http.MethodPost,
			path: "/api/admin/session/parties/1/disband", roles: []string{"viewer"},
			status: http.StatusForbidden, code: auth.ReasonInsufficientRole,
		},
		{
			name: "live_ops on the force-disband route", method: http.MethodPost,
			path: "/api/admin/session/parties/1/disband", roles: []string{"live_ops"},
			status: http.StatusNotImplemented, code: "not_implemented",
		},
		{
			name: "live_ops on a read route", method: http.MethodGet,
			path: "/api/admin/session/players", roles: []string{"live_ops"},
			status: http.StatusNotImplemented, code: "not_implemented",
		},
		{
			// An unrecognised role grants nothing, so a token carrying only one is
			// refused rather than admitted on the strength of being a staff token.
			name: "an unknown role", method: http.MethodGet,
			path: "/api/admin/session/players", roles: []string{"root"},
			status: http.StatusForbidden, code: auth.ReasonInsufficientRole,
		},
		{
			name: "admin, with an unknown role alongside", method: http.MethodPost,
			path: "/api/admin/session/parties/1/disband", roles: []string{"root", "admin"},
			status: http.StatusNotImplemented, code: "not_implemented",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _, body := h.request(t, tt.method, tt.path, h.staffToken(t, tt.roles...))
			if status != tt.status {
				t.Fatalf("status = %d, want %d (body: %s)", status, tt.status, body)
			}
			if code := errorCode(t, body); code != tt.code {
				t.Errorf("code = %q, want %q", code, tt.code)
			}
		})
	}
}

// TestAPlayerTokenReachesItsHandler is the positive case: the guard lets a good player
// token through, and what answers is the stub. Without this, every test above would pass
// on a service that refused everything.
func TestAPlayerTokenReachesItsHandler(t *testing.T) {
	h := newHarness(t, nil)
	token := h.playerToken(t)

	for _, rt := range api.Routes() {
		if rt.Group != api.GroupPlayer {
			continue
		}
		t.Run(rt.Pattern(), func(t *testing.T) {
			path := strings.NewReplacer(
				"{player_id}", otherID, "{invite_id}", "1",
			).Replace(rt.Path)

			status, _, body := h.request(t, rt.Method, path, token)
			if status != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501 (body: %s)", status, body)
			}
			if code := errorCode(t, body); code != "not_implemented" {
				t.Errorf("code = %q, want not_implemented", code)
			}
		})
	}
}

// TestTheFallbackIsGuardedLikeEveryRoute is the property that keeps the route table
// private. An unauthenticated caller must not be able to tell a real path from a
// misspelling, so both are answered with the same 401.
func TestTheFallbackIsGuardedLikeEveryRoute(t *testing.T) {
	h := newHarness(t, nil)

	t.Run("unauthenticated", func(t *testing.T) {
		for _, path := range []string{"/", "/nope", "/api/player/session/nope", "/api/admin/session/nope"} {
			status, _, body := h.request(t, http.MethodGet, path, "")
			if status != http.StatusUnauthorized {
				t.Errorf("GET %s: status = %d, want 401", path, status)
			}
			if code := errorCode(t, body); code != auth.ReasonMissingToken {
				t.Errorf("GET %s: code = %q, want %q", path, code, auth.ReasonMissingToken)
			}
		}
	})

	t.Run("with a player token", func(t *testing.T) {
		status, _, body := h.request(t, http.MethodGet, "/nope", h.playerToken(t))
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", status)
		}
		if code := errorCode(t, body); code != "not_found" {
			t.Errorf("code = %q, want not_found", code)
		}
		if allow := errorCode(t, body); allow == "" {
			t.Error("the 404 body carries no code")
		}
	})

	t.Run("a staff token does not open the player fallback", func(t *testing.T) {
		status, _, _ := h.request(t, http.MethodGet, "/nope", h.staffToken(t, "admin"))
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})

	t.Run("the internal listener is not reachable here", func(t *testing.T) {
		// /metrics and /healthz exist on the other listener only, so on this one they are
		// just two more unmatched paths.
		for _, path := range []string{"/metrics", "/healthz", "/readyz", "/debug/pprof/"} {
			status, _, _ := h.request(t, http.MethodGet, path, h.playerToken(t))
			if status != http.StatusNotFound {
				t.Errorf("GET %s: status = %d, want 404", path, status)
			}
		}
	})
}

// TestMethodMismatchIsAnsweredByTheFallback pins that a 405 comes from this service
// rather than from net/http's own mux. Both would be a 405, but only ours carries the
// COM-5 envelope — and an error body that is plain text on one path and JSON on the rest
// is the kind of inconsistency a client discovers in production.
func TestMethodMismatchIsAnsweredByTheFallback(t *testing.T) {
	h := newHarness(t, nil)

	status, header, body := h.request(t, http.MethodDelete, "/api/player/session/me", h.playerToken(t))
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (body: %s)", status, body)
	}
	if got := header.Get("Allow"); got != "GET, HEAD, PATCH" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD, PATCH")
	}
	if ct := header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q; net/http's own 405 would be plain text", ct)
	}
	if code := errorCode(t, body); code != "method_not_allowed" {
		t.Errorf("code = %q, want method_not_allowed", code)
	}
}

// TestRequestIDIsEchoedAndMinted covers both halves of the request ID: Gateway's value is
// preserved so a trace spans services, and one is minted when it is absent so a log line
// still has something to group by.
func TestRequestIDIsEchoedAndMinted(t *testing.T) {
	h := newHarness(t, nil)

	t.Run("an inbound id is reused", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, h.publicURL+"/api/player/session/me", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("X-Request-Id", "from-gateway")
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if got := resp.Header.Get("X-Request-Id"); got != "from-gateway" {
			t.Errorf("X-Request-Id = %q, want the inbound value", got)
		}
		var env struct {
			Error struct {
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("body is not the envelope: %s", body)
		}
		if env.Error.RequestID != "from-gateway" {
			t.Errorf("error.request_id = %q, want the inbound value", env.Error.RequestID)
		}
	})

	t.Run("an id is minted when none arrives", func(t *testing.T) {
		_, header, body := h.request(t, http.MethodGet, "/api/player/session/me", "")

		id := header.Get("X-Request-Id")
		if id == "" {
			t.Fatal("no X-Request-Id on the response")
		}
		var env struct {
			Error struct {
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("body is not the envelope: %s", body)
		}
		if env.Error.RequestID != id {
			t.Errorf("error.request_id = %q, response header = %q; they must agree", env.Error.RequestID, id)
		}
	})
}

// TestHealthAndReadiness covers the internal listener, which has no middleware in front of
// it and therefore behaves differently from everything above: no token, no request ID, no
// COM-5 request_id field.
func TestHealthAndReadiness(t *testing.T) {
	boom := errors.New("postgres unreachable")
	failing := newHarness(t, func(d *Deps) {
		d.Ready = func(context.Context) error { return boom }
	})

	t.Run("healthz answers before the service is ready", func(t *testing.T) {
		status, body := failing.scrape(t, "/healthz")
		if status != http.StatusOK || body != "ok" {
			t.Fatalf("GET /healthz = %d %q, want 200 ok", status, body)
		}
	})

	t.Run("readyz is 503 until the caller says otherwise", func(t *testing.T) {
		status, body := failing.scrape(t, "/readyz")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body: %s)", status, body)
		}
		if code := errorCode(t, body); code != "not_ready" {
			t.Errorf("code = %q, want not_ready", code)
		}
		if !strings.Contains(body, "still starting up") {
			t.Errorf("body does not say why: %s", body)
		}
	})

	t.Run("readyz reports the dependency's own error", func(t *testing.T) {
		failing.SetReady(true)
		status, body := failing.scrape(t, "/readyz")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body: %s)", status, body)
		}
		// The message is the check's, not a generic one: an operator reading /readyz
		// should not have to go to the logs to learn which dependency is down.
		if !strings.Contains(body, boom.Error()) {
			t.Errorf("body = %s, want it to carry the check's message", body)
		}
	})

	t.Run("readyz is 200 once ready", func(t *testing.T) {
		healthy := newHarness(t, func(d *Deps) {
			d.Ready = func(context.Context) error { return nil }
		})
		healthy.SetReady(true)

		status, body := healthy.scrape(t, "/readyz")
		if status != http.StatusOK || body != "ok" {
			t.Fatalf("GET /readyz = %d %q, want 200 ok", status, body)
		}
	})

	t.Run("a nil check means nothing to verify", func(t *testing.T) {
		// Deps.Ready is optional — the tests rely on it, and so would a deployment whose
		// readiness is entirely its own listener being up.
		h := newHarness(t, nil)
		h.SetReady(true)

		status, body := h.scrape(t, "/readyz")
		if status != http.StatusOK || body != "ok" {
			t.Fatalf("GET /readyz = %d %q, want 200 ok", status, body)
		}
	})
}

// TestReadyzSurvivesAHalfClosedProbe reproduces a readiness bug. deploy/scripts/lib.sh probes
// readiness with `printf 'GET ...' | nc`, and nc shuts down its write side as soon as
// printf ends. Go's server reads that EOF as the client going away and cancels the request
// context, so a check that honoured it failed with "context canceled" while Postgres and
// Valkey were both fine. The check must see a context that only its own deadline can end.
func TestReadyzSurvivesAHalfClosedProbe(t *testing.T) {
	// The check behaves like a real network ping: it takes a moment, and it gives up as
	// soon as its context is done.
	h := newHarness(t, func(d *Deps) {
		d.Ready = func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
				return nil
			}
		}
	})
	h.SetReady(true)

	conn, err := net.Dial("tcp", h.MetricsAddr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := io.WriteString(conn, "GET /readyz HTTP/1.0\r\nHost: session\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("GET /readyz after a half-close = %d %q, want 200 ok", resp.StatusCode, body)
	}
}

// TestReadyzCheckHasItsOwnDeadline checks the other half of the fix: detaching from the
// request context must not let a hung dependency hold /readyz open for ever.
func TestReadyzCheckHasItsOwnDeadline(t *testing.T) {
	h := newHarness(t, func(d *Deps) {
		d.Ready = func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("the readiness check was given no deadline")
			}
			<-ctx.Done()
			return ctx.Err()
		}
	})
	h.SetReady(true)

	start := time.Now()
	status, body := h.scrape(t, "/readyz")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", status, body)
	}
	if !strings.Contains(body, context.DeadlineExceeded.Error()) {
		t.Errorf("body = %s, want the deadline to be the reason", body)
	}
	if took := time.Since(start); took > api.ReadyCheckTimeout+2*time.Second {
		t.Errorf("/readyz took %v, want it bounded by %v", took, api.ReadyCheckTimeout)
	}
}

// TestMetricsRecordTheRequestAndTheReason is the check that the two things an operator
// needs to correlate — which route is failing, and why tokens are being refused — are
// actually exported, with the labels COM-10 asks for.
func TestMetricsRecordTheRequestAndTheReason(t *testing.T) {
	h := newHarness(t, nil)

	// One authenticated request and one refused for a missing token, so both counters
	// have a sample.
	if status, _, _ := h.request(t, http.MethodGet, "/api/player/session/me", h.playerToken(t)); status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", status)
	}
	if status, _, _ := h.request(t, http.MethodGet, "/api/player/session/friends", ""); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if status, _, _ := h.request(t, http.MethodGet, "/api/admin/session/players", h.staffToken(t, "viewer")); status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", status)
	}

	status, body := h.scrape(t, "/metrics")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d", status)
	}

	for _, want := range []string{
		// The route label is the pattern with the method stripped, per COM-10, and the
		// method and status are their own labels.
		`session_http_requests_total{method="GET",route="/api/player/session/me",status="501"}`,
		`session_http_requests_total{method="GET",route="/api/player/session/friends",status="401"}`,
		`session_http_request_duration_seconds_count{method="GET",route="/api/player/session/me"}`,
		// domain is this service's addition to COM-10: the same reason on two different
		// domains sends an operator to two different issuers.
		`session_token_rejected_total{domain="player",reason="missing_token"}`,
		`session_build_info{version="test"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics are missing %s", want)
		}
	}

	if strings.Contains(body, playerID) || strings.Contains(body, otherID) {
		t.Error("a player id reached the metrics; no label may carry one")
	}
}

// TestPanicBecomesA500AndIsStillCounted covers the recovery path, which no route reaches
// today — a handler has to panic first, and none of the stubs do. It is tested directly for
// that reason: the middleware's value is exactly in the case nothing else exercises.
func TestPanicBecomesA500AndIsStillCounted(t *testing.T) {
	h := newHarness(t, nil)

	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	})
	// The same nesting publicHandler uses, so the request ID is available to the 500 body
	// and the failure is counted rather than vanishing.
	handler := withRequestID(h.metrics.middleware(h.withRecover(panicking)))

	req := httptest.NewRequest(http.MethodGet, "/api/player/session/me", nil)
	req = req.WithContext(api.WithRequestID(req.Context(), "panic-id"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := errorCode(t, rec.Body.String()); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
	// The panic's text must not travel to the client.
	if strings.Contains(rec.Body.String(), "handler exploded") {
		t.Error("the panic message reached the response body")
	}

	_, body := h.scrape(t, "/metrics")
	if !strings.Contains(body, `session_http_requests_total{method="GET",route="unmatched",status="500"}`) {
		t.Error("the panicking request was not counted")
	}
}

// TestAMissingVerifierRefusesItsRoutes is the fail-closed direction of Deps: a server
// built without a verifier cannot check that group's tokens, and a 500 is the honest
// answer. Answering 501 — or worse, letting the request through — would make a wiring
// mistake look like a working deploy.
func TestAMissingVerifierRefusesItsRoutes(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.StaffVerifier = nil })

	status, _, body := h.request(t, http.MethodGet, "/api/admin/session/players", h.staffToken(t, "admin"))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", status, body)
	}
	if code := errorCode(t, body); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}

	// The player domain is unaffected, which is the point of two separate fields.
	if status, _, _ := h.request(t, http.MethodGet, "/api/player/session/me", h.playerToken(t)); status != http.StatusNotImplemented {
		t.Errorf("the player domain broke too: status = %d, want 501", status)
	}
}

// TestRunReportsAPortInUse pins that binding happens before the goroutines start: a port
// already taken has to come back as an error from Run, not as a listener that dies
// asynchronously while the process reports itself healthy.
func TestRunReportsAPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	s := New(config.Config{
		ListenAddr:      ln.Addr().String(),
		MetricsAddr:     "127.0.0.1:0",
		InternalAddr:    "127.0.0.1:0",
		ReadTimeout:     time.Second,
		WriteTimeout:    time.Second,
		IdleTimeout:     time.Second,
		ShutdownTimeout: time.Second,
	}, Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	if err := s.Run(t.Context()); err == nil {
		t.Fatal("Run accepted a port that is already bound")
	}
	select {
	case <-s.Started():
		t.Error("Started was closed by a Run that failed to bind")
	default:
	}
}

// TestRunStopsWhenTheContextIsCancelled is the shutdown contract: Run returns once the
// context is done, without the caller having to close anything.
func TestRunStopsWhenTheContextIsCancelled(t *testing.T) {
	s := New(config.Config{
		ListenAddr:      "127.0.0.1:0",
		MetricsAddr:     "127.0.0.1:0",
		InternalAddr:    "127.0.0.1:0",
		ReadTimeout:     time.Second,
		WriteTimeout:    time.Second,
		IdleTimeout:     time.Second,
		ShutdownTimeout: 2 * time.Second,
	}, Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	select {
	case <-s.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("the listeners never started")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestRouteLabel(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		{"", "unmatched"},
		{"/", "unmatched"},
		{"GET /api/player/session/me", "/api/player/session/me"},
		{"POST /api/player/session/friends/requests/{player_id}/accept", "/api/player/session/friends/requests/{player_id}/accept"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Pattern = tt.pattern
		if got := routeLabel(r); got != tt.want {
			t.Errorf("routeLabel(%q) = %q, want %q", tt.pattern, got, tt.want)
		}
	}
}

func TestStatusLabel(t *testing.T) {
	if got := statusLabel(http.StatusServiceUnavailable); got != "503" {
		t.Errorf("statusLabel(503) = %q", got)
	}
}

// TestRecordWriterRemembersAnImplicitStatus covers the handler that writes a body and
// never calls WriteHeader — net/http makes that a 200, and a recordWriter that reported 0
// would put the request in the metrics under a status no client ever saw.
func TestRecordWriterRemembersAnImplicitStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &recordWriter{ResponseWriter: rec}
	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if w.status != http.StatusOK {
		t.Errorf("status = %d, want 200", w.status)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("underlying status = %d, want 200", rec.Code)
	}
	if w.Unwrap() != http.ResponseWriter(rec) {
		t.Error("Unwrap does not return the wrapped writer")
	}
}

// TestRecordWriterKeepsTheFirstStatus pins that a second WriteHeader does not overwrite
// the recorded one: the status that counts is the one the client received.
func TestRecordWriterKeepsTheFirstStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &recordWriter{ResponseWriter: rec}
	w.WriteHeader(http.StatusUnauthorized)
	w.WriteHeader(http.StatusTeapot)

	if w.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.status)
	}
}
