package authn

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"gateway/gateway/internal/router"
)

// The tests below are the acceptance list from techspec §6.3, §8 and §9, one
// case per bullet. Each asserts the status *and* the COM-5 code, because the
// code is what gateway_token_rejected_total{reason} counts — a case that returns
// the right status under the wrong reason is a case that makes the Dashboard
// mislead.

// --- §6.3 acceptance: token validity ---

func TestValidPlayerTokenIsAccepted(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	tok := keys.sign(t, validPlayerClaims())
	w := serve(t, mw, playerRoute, bearer(tok))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
}

func TestMissingTokenIsRejected(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	for _, header := range []string{"", "Token abc", "Bearer", "Bearer "} {
		t.Run("header="+header, func(t *testing.T) {
			w := serve(t, mw, playerRoute, header)
			assertRejected(t, w, http.StatusUnauthorized, reasonMissingToken)
		})
	}
}

// A token signed with a key that is not in the relevant JWKS — the self-signed
// case in §6.3.
func TestSelfSignedTokenIsRejected(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	attacker := newTestJWKS(t, "player-key-1") // same kid, different key material
	tok := attacker.sign(t, validPlayerClaims())
	w := serve(t, mw, playerRoute, bearer(tok))

	assertRejected(t, w, http.StatusUnauthorized, reasonInvalidSignature)
}

// Algorithm substitution: a token whose header claims `none` or HS256 must be
// rejected by WithValidMethods before any key lookup happens (§6.3).
func TestAlgorithmSubstitutionIsRejected(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	tests := map[string]func(t *testing.T) string{
		"alg=none": func(t *testing.T) string {
			tok := jwt.NewWithClaims(jwt.SigningMethodNone, validPlayerClaims())
			tok.Header["kid"] = "player-key-1"
			s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"alg=HS256 keyed with the public key": func(t *testing.T) string {
			pub := keys.priv.Public().(ed25519.PublicKey)
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validPlayerClaims())
			tok.Header["kid"] = "player-key-1"
			// The classic confusion attack: sign HMAC with the public key and hope
			// the verifier feeds it back as an HMAC secret.
			s, err := tok.SignedString([]byte(base64.RawURLEncoding.EncodeToString(pub)))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}

	for name, mint := range tests {
		t.Run(name, func(t *testing.T) {
			w := serve(t, mw, playerRoute, bearer(mint(t)))
			assertRejected(t, w, http.StatusUnauthorized, reasonInvalidSignature)
		})
	}
}

func TestExpiredTokenIsRejectedOutsideSkew(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	claims := validPlayerClaims()
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
	w := serve(t, mw, playerRoute, bearer(keys.sign(t, claims)))

	assertRejected(t, w, http.StatusUnauthorized, reasonExpired)
}

// Clock skew: expired by less than GATEWAY_JWT_CLOCK_SKEW is accepted, because
// VM clocks drift and a 15-minute TTL makes 30s of leeway a small fraction of it.
func TestExpiredWithinClockSkewIsAccepted(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	claims := validPlayerClaims()
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-10 * time.Second))
	w := serve(t, mw, playerRoute, bearer(keys.sign(t, claims)))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — 10s past exp is inside the 30s skew (body %s)", w.Code, w.Body.String())
	}
}

// A token with no exp at all is rejected: WithExpirationRequired is what stops a
// token that never expires from being the one that outlives a revocation.
func TestTokenWithoutExpiryIsRejected(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	claims := validPlayerClaims()
	claims.ExpiresAt = nil
	w := serve(t, mw, playerRoute, bearer(keys.sign(t, claims)))

	assertRejected(t, w, http.StatusUnauthorized, reasonInvalidToken)
}

// --- §6.3/§8 acceptance: cross-domain rejection ---

// The headline case in the identity contract: a correctly signed player token on
// a staff route is a 401.
//
// Note the reason is invalid_signature, not iss_mismatch, and that is worth
// being precise about — techspec §6.3 describes cross-domain rejection as the
// iss/aud checks doing the work. In practice the two domains publish different
// `kid`s, so the token dies earlier, at key selection: the staff JWKS has no key
// for the player token's kid, nothing verifies it, and the claim checks never
// run. The layering is kid → signature → iss → aud, and each layer is the
// backstop for the one before it. A same-`kid` collision still fails at the
// signature, and only a genuinely shared key would reach the issuer check — see
// TestWrongIssuerIsRejected for that last layer on its own.
func TestPlayerTokenOnStaffRouteIsRejected(t *testing.T) {
	player := newTestJWKS(t, "player-key-1")
	staff := newTestJWKS(t, "staff-key-1")
	reg := testRegistry(t, map[router.Group]string{
		router.GroupPlayer: player.url(),
		router.GroupStaff:  staff.url(),
	})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer, router.GroupStaff), 30*time.Second)

	tok := player.sign(t, validPlayerClaims())
	w := serve(t, mw, staffRoute, bearer(tok))

	assertRejected(t, w, http.StatusUnauthorized, reasonInvalidSignature)
}

// And the mirror: a staff token on a player route.
func TestStaffTokenOnPlayerRouteIsRejected(t *testing.T) {
	player := newTestJWKS(t, "player-key-1")
	staff := newTestJWKS(t, "staff-key-1")
	reg := testRegistry(t, map[router.Group]string{
		router.GroupPlayer: player.url(),
		router.GroupStaff:  staff.url(),
	})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer, router.GroupStaff), 30*time.Second)

	tok := staff.sign(t, validStaffClaims("admin"))
	w := serve(t, mw, playerRoute, bearer(tok))

	assertRejected(t, w, http.StatusUnauthorized, reasonInvalidSignature)
}

// The last line of defence, reachable only when the key resolves and the
// signature verifies: one JWKS shared by both domains, so only the claim checks
// are left to reject the token. Asserts the issuer reason specifically, because
// the token is wrong on both iss and aud and the issuer is the claim that names
// the domain it came from.
func TestSharedKeyCrossDomainIsRejectedOnIssuer(t *testing.T) {
	shared := newTestJWKS(t, "shared-key-1")
	reg := testRegistry(t, map[router.Group]string{
		router.GroupPlayer: shared.url(),
		router.GroupStaff:  shared.url(),
	})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer, router.GroupStaff), 30*time.Second)

	claims := validPlayerClaims() // player iss/aud, signed by the key both domains publish
	w := serve(t, mw, staffRoute, bearer(shared.sign(t, claims)))

	assertRejected(t, w, http.StatusUnauthorized, reasonIssuerMismatch)
}

// Right issuer, wrong audience: a token minted for another service by the same
// issuer. Distinct reason so it is debuggable without reading raw logs (§8).
func TestWrongAudienceIsRejected(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	claims := validPlayerClaims()
	claims.Audience = jwt.ClaimStrings{"otomo:something-else"}
	w := serve(t, mw, playerRoute, bearer(keys.sign(t, claims)))

	assertRejected(t, w, http.StatusUnauthorized, reasonAudienceMismatch)
}

// Right audience, wrong issuer.
func TestWrongIssuerIsRejected(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	claims := validPlayerClaims()
	claims.Issuer = "https://evil.example"
	w := serve(t, mw, playerRoute, bearer(keys.sign(t, claims)))

	assertRejected(t, w, http.StatusUnauthorized, reasonIssuerMismatch)
}

// --- §8 acceptance: role hierarchy ---

func TestRoleHierarchy(t *testing.T) {
	staff := newTestJWKS(t, "staff-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupStaff: staff.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupStaff), 30*time.Second)

	viewerRoute := staffRoute
	viewerRoute.MinRole = router.RoleViewer
	adminRoute := staffRoute
	adminRoute.MinRole = router.RoleAdmin

	tests := []struct {
		name       string
		roles      []string
		route      router.Route
		wantStatus int
		wantReason string
	}{
		{"admin passes RoleLiveOps", []string{"admin"}, staffRoute, http.StatusOK, ""},
		{"live_ops passes RoleLiveOps", []string{"live_ops"}, staffRoute, http.StatusOK, ""},
		{"viewer fails RoleLiveOps", []string{"viewer"}, staffRoute, http.StatusForbidden, reasonInsufficientRole},
		{"live_ops fails RoleAdmin", []string{"live_ops"}, adminRoute, http.StatusForbidden, reasonInsufficientRole},
		{"admin passes RoleViewer", []string{"admin"}, viewerRoute, http.StatusOK, ""},
		// The check takes the highest ordinal present, not the first or the last.
		{"multiple roles use the highest", []string{"viewer", "admin"}, staffRoute, http.StatusOK, ""},
		{"an unknown role grants nothing", []string{"superuser"}, staffRoute, http.StatusForbidden, reasonInsufficientRole},
		// A staff token with no roles at all is valid but authorized for nothing.
		{"no roles fails any MinRole", nil, staffRoute, http.StatusForbidden, reasonInsufficientRole},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tok := staff.sign(t, validStaffClaims(tc.roles...))
			w := serve(t, mw, tc.route, bearer(tok))
			if tc.wantReason == "" {
				if w.Code != tc.wantStatus {
					t.Errorf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
				}
				return
			}
			assertRejected(t, w, tc.wantStatus, tc.wantReason)
		})
	}
}

// A player token carries no roles claim at all, and player routes set MinRole 0,
// so the absence of roles must not be read as "denied" there (contract §4).
func TestPlayerTokenWithNoRolesPassesPlayerRoute(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	tok := keys.sign(t, validPlayerClaims())
	if w := serve(t, mw, playerRoute, bearer(tok)); w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
}

// --- §9 acceptance: public routes are untouched ---

func TestPublicRouteSkipsAuthentication(t *testing.T) {
	// No registry at all: a public route must not need one.
	mw := RequireGroup(NewRegistry(nil), standardIssuers(), 30*time.Second)

	for _, pattern := range []string{"/auth/", "/patch/v1/live/manifest"} {
		route := router.Route{Method: "*", Pattern: pattern, Group: router.GroupPublic}
		if w := serve(t, mw, route, ""); w.Code != http.StatusOK {
			t.Errorf("GET %s without a token = %d, want 200", pattern, w.Code)
		}
	}
}

// A protected route whose domain has no keys yet — the window between Gateway
// starting and Auth's first JWKS response — must reject, not pass through.
func TestProtectedRouteBeforeJWKSFetchRejectsEverything(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := NewRegistry(map[router.Group]string{router.GroupPlayer: keys.url()})
	// Deliberately not started: no keys have been fetched.
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	tok := keys.sign(t, validPlayerClaims())
	w := serve(t, mw, playerRoute, bearer(tok))
	assertRejected(t, w, http.StatusUnauthorized, reasonInvalidToken)
}

// A route whose group has no configured issuer is a startup-validation failure.
// If it is ever reached, it must 500 rather than serve unauthenticated.
func TestProtectedRouteWithNoIssuerFailsClosed(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, map[router.Group]Issuer{}, 30*time.Second)

	tok := keys.sign(t, validPlayerClaims())
	w := serve(t, mw, playerRoute, bearer(tok))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if got := rejectionCode(t, w); got != "internal_error" {
		t.Errorf("COM-5 code = %q, want internal_error", got)
	}
}

// --- claims plumbing ---

// The verified claims must reach the next handler without a second parse: this
// is where Session gets `sub` from.
func TestVerifiedClaimsReachTheNextHandler(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	var got *Claims
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = ClaimsFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, playerRoute.Pattern, nil)
	req.Header.Set("Authorization", bearer(keys.sign(t, validPlayerClaims())))
	mw(playerRoute, next).ServeHTTP(httptest.NewRecorder(), req)

	if got == nil {
		t.Fatal("claims not on the context")
	}
	if got.Subject != "player-uuid-1234" {
		t.Errorf("sub = %q, want player-uuid-1234", got.Subject)
	}
	if got.Issuer != playerIssuer {
		t.Errorf("iss = %q, want %q", got.Issuer, playerIssuer)
	}
}

// A token without `sub` is well-formed but unusable downstream — Session keys
// its whole model on it (contract §4). Reject at the boundary, where the
// difference is still visible.
func TestTokenWithoutSubjectIsRejected(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := testRegistry(t, map[router.Group]string{router.GroupPlayer: keys.url()})
	mw := RequireGroup(reg, standardIssuers(router.GroupPlayer), 30*time.Second)

	claims := validPlayerClaims()
	claims.Subject = ""
	w := serve(t, mw, playerRoute, bearer(keys.sign(t, claims)))

	assertRejected(t, w, http.StatusUnauthorized, reasonInvalidToken)
}

// --- registry readiness ---

func TestRegistryReportsNotReadyUntilKeysArrive(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	reg := NewRegistry(map[router.Group]string{router.GroupPlayer: keys.url()})

	if reg.Ready(t.Context()) {
		t.Fatal("a registry that has not fetched yet reports ready")
	}
	if got := reg.Status(t.Context()); got["player"] {
		t.Errorf("status[player] = true before any fetch, want false")
	}

	reg.Start(t.Context())
	waitReady(t, reg, true)

	if keys.fetchCount() == 0 {
		t.Error("registry reports ready but never fetched the JWKS")
	}
}

// A JWKS endpoint that answers but refuses to serve keys keeps /readyz at 503:
// readiness is about having keys, not about having reached the host.
func TestRegistryNotReadyWhenJWKSRefusesToServeKeys(t *testing.T) {
	keys := newTestJWKS(t, "player-key-1")
	keys.unauthorized = true

	reg := NewRegistry(map[router.Group]string{router.GroupPlayer: keys.url()})
	reg.Start(t.Context())

	// Give the first fetch time to land and fail.
	time.Sleep(200 * time.Millisecond)
	if reg.Ready(t.Context()) {
		t.Error("registry reports ready against a JWKS that serves no keys")
	}
}

// Two domains: readiness requires both, and one being up does not cover for the
// other. This is the load-bearing consequence of keeping the dev/staging
// manifest routes here: a PHP Admin Auth outage holds this gateway at 503 even
// though every player route could still serve.
func TestRegistryReadyRequiresEveryDomain(t *testing.T) {
	player := newTestJWKS(t, "player-key-1")
	staff := newTestJWKS(t, "staff-key-1")
	staff.unauthorized = true

	reg := NewRegistry(map[router.Group]string{
		router.GroupPlayer: player.url(),
		router.GroupStaff:  staff.url(),
	})
	reg.Start(t.Context())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.Status(t.Context())["player"] {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !reg.Status(t.Context())["player"] {
		t.Fatal("the player domain never became ready")
	}
	if reg.Ready(t.Context()) {
		t.Error("registry reports ready with one domain unready")
	}
}

// --- route table policy (techspec §9) ---

// Cheap regression guard: nothing under /api/ may ever be reachable without a
// token. Scoped to /api/ rather than to the whole table because /auth/,
// /patch/v1/live/manifest and /patch/v1/blob/ are public by design.
func TestNoPublicRouteUnderAPI(t *testing.T) {
	for _, r := range router.BuildRoutes() {
		if r.Group == router.GroupPublic && strings.HasPrefix(r.Pattern, "/api/") {
			t.Errorf("route %s is GroupPublic; every /api/ route must be authenticated",
				r.MuxPattern())
		}
	}
}

// This table uses both domains, and that is deliberate rather than sloppy: the
// two dev/staging manifest routes take a staff token (design/03-patch-minimal.md
// PAT-B6), so this service loads a second issuer and /readyz waits on it. A
// table that reported only the player domain would mean those two routes had
// lost their staff requirement. services/gateway_dev asserts the mirror image:
// its table uses the staff domain and nothing else.
//
// Every domain found must also be one the registry knows how to build, so that
// main's verifyIssuers finds a URL and an issuer for each.
func TestUsedGroupsMatchesTheTable(t *testing.T) {
	used := router.UsedGroups(router.BuildRoutes())
	if !used[router.GroupPlayer] {
		t.Errorf("table protects nothing in the player domain: %v", used)
	}
	if !used[router.GroupStaff] {
		t.Errorf("table should use the staff domain for the dev/staging manifests: %v", used)
	}
	for g := range used {
		if _, ok := standardIssuers(g)[g]; !ok {
			t.Errorf("table uses domain %v, which has no issuer mapping", g)
		}
	}
}

// --- reason mapping ---

// reasonFor is the switch that decides which metric bucket a failure lands in,
// so it is asserted directly as well as through the HTTP cases above.
func TestReasonFor(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
	}{
		"expired":            {jwt.ErrTokenExpired, reasonExpired},
		"bad audience":       {jwt.ErrTokenInvalidAudience, reasonAudienceMismatch},
		"bad issuer":         {jwt.ErrTokenInvalidIssuer, reasonIssuerMismatch},
		"bad signature":      {jwt.ErrTokenSignatureInvalid, reasonInvalidSignature},
		"unverifiable":       {jwt.ErrTokenUnverifiable, reasonInvalidSignature},
		"malformed":          {jwt.ErrTokenMalformed, reasonInvalidToken},
		"not yet valid":      {jwt.ErrTokenNotValidYet, reasonInvalidToken},
		"nil error":          {nil, reasonInvalidToken},
		"unrecognised error": {json.Unmarshal([]byte("{"), &struct{}{}), reasonInvalidToken},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := reasonFor(tc.err); got != tc.want {
				t.Errorf("reasonFor(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// A signature failure outranks an expiry failure in the label: a forged token
// that is also stale should not be reported as merely expired.
func TestSignatureFailureOutranksExpiry(t *testing.T) {
	joined := errors.Join(jwt.ErrTokenExpired, jwt.ErrTokenSignatureInvalid)
	if got := reasonFor(joined); got != reasonInvalidSignature {
		t.Errorf("reasonFor(expired+invalid signature) = %q, want %q", got, reasonInvalidSignature)
	}
}

// A token from the wrong domain fails both claim checks, and the issuer is what
// gets reported — see the reasoning on reasonFor.
func TestIssuerOutranksAudience(t *testing.T) {
	joined := errors.Join(jwt.ErrTokenInvalidAudience, jwt.ErrTokenInvalidIssuer)
	if got := reasonFor(joined); got != reasonIssuerMismatch {
		t.Errorf("reasonFor(bad audience+bad issuer) = %q, want %q", got, reasonIssuerMismatch)
	}
}
