package authn

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"gateway/gateway/internal/router"
)

// This file is the fixture generator from 06-auth-identity-contract.md §10. It
// stands up a JWKS endpoint with an ephemeral Ed25519 key and signs tokens
// against it, so every acceptance case in techspec §6.3 and §8 can be tested
// without Auth or admin-auth running — which is the point: the middleware's correctness
// must not depend on another team's deploy schedule.

const (
	playerIssuer   = "https://auth.otomo.internal"
	playerAudience = "otomo:player"
	// admin-auth, not staff-auth: design/06-auth-identity-contract.md:107 and
	// design/05-gateway-techspec.md:78 both fix the staff issuer to
	// https://admin-auth.otomo.internal. The fixture said staff-auth, a string
	// that appears in no document, so the tests never exercised the real name.
	staffIssuer   = "https://admin-auth.otomo.internal"
	staffAudience = "otomo:staff"
)

// testJWKS is one ephemeral keypair published over HTTP, plus a counter of how
// many times it has been fetched.
type testJWKS struct {
	*httptest.Server
	kid     string
	priv    ed25519.PrivateKey
	mu      sync.Mutex
	fetches int
	// unauthorized makes the endpoint answer 401, simulating a JWKS that exists
	// but refuses to serve keys.
	unauthorized bool
}

// newTestJWKS serves one Ed25519 public key as a JWK Set at "/.well-known/jwks.json".
func newTestJWKS(t *testing.T, kid string) *testJWKS {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	j := &testJWKS{kid: kid, priv: priv}

	j.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		j.mu.Lock()
		j.fetches++
		unauthorized := j.unauthorized
		j.mu.Unlock()

		if unauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "OKP",
				"crv": "Ed25519",
				"kid": kid,
				"x":   base64.RawURLEncoding.EncodeToString(pub),
				"use": "sig",
				"alg": "EdDSA",
			}},
		})
	}))
	t.Cleanup(j.Close)
	return j
}

// keysPath is the path the registry fetches. The registry is given a full URL so
// the path is arbitrary; this keeps it shaped like the real contract.
const keysPath = "/.well-known/jwks.json"

func (j *testJWKS) url() string { return j.Server.URL + keysPath }

// fetchCount reports how many times the JWKS document has been requested.
func (j *testJWKS) fetchCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.fetches
}

// sign mints a token signed by this keypair, with kid in the header so the
// verifier can select the key the way it would in production.
func (j *testJWKS) sign(t *testing.T, claims Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = j.kid
	s, err := tok.SignedString(j.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// validPlayerClaims is a well-formed player token: right issuer, right audience,
// a subject, and an expiry far enough out to be unambiguous.
func validPlayerClaims() Claims {
	now := time.Now()
	return Claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer:    playerIssuer,
		Audience:  jwt.ClaimStrings{playerAudience},
		Subject:   "player-uuid-1234",
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
	}}
}

// validStaffClaims is the same for the staff domain, carrying one role.
func validStaffClaims(roles ...string) Claims {
	now := time.Now()
	return Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    staffIssuer,
			Audience:  jwt.ClaimStrings{staffAudience},
			Subject:   "staff-uuid-5678",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
		},
		Roles: roles,
	}
}

// testRegistry starts a registry against the given endpoints and blocks until
// each domain has keys, so tests never race the first fetch.
//
// It runs on the test's own context, which the testing package cancels when the
// test ends — that stops the clients' refresh goroutines, so nothing outlives
// the test that started it.
func testRegistry(t *testing.T, urls map[router.Group]string) *Registry {
	t.Helper()
	reg := NewRegistry(urls)
	reg.Start(t.Context())
	waitReady(t, reg, true)
	return reg
}

// waitReady blocks until the registry's readiness reaches want, failing the test
// if it does not get there.
func waitReady(t *testing.T, reg *Registry, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if reg.Ready(t.Context()) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("registry readiness never became %v (status %v)", want, reg.Status(t.Context()))
}

// standardIssuers is the domain map main builds for the given groups.
func standardIssuers(groups ...router.Group) map[router.Group]Issuer {
	out := make(map[router.Group]Issuer, len(groups))
	for _, g := range groups {
		switch g {
		case router.GroupPlayer:
			out[g] = Issuer{Issuer: playerIssuer, Audience: playerAudience}
		case router.GroupStaff:
			out[g] = Issuer{Issuer: staffIssuer, Audience: staffAudience}
		}
	}
	return out
}

// serve runs one request through the middleware for a route and returns the
// recorder, so each test reads as: build request, assert status and reason.
func serve(t *testing.T, mw Middleware, route router.Route, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, route.Pattern, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	mw(route, next).ServeHTTP(w, req)
	if w.Code == http.StatusOK && !reached {
		t.Fatal("middleware returned 200 without calling the next handler")
	}
	return w
}

// rejectionCode pulls the COM-5 error code out of a rejection response.
func rejectionCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decoding COM-5 body: %v", err)
	}
	return body.Error.Code
}

// bearer renders an Authorization header value.
func bearer(tok string) string { return "Bearer " + tok }

// playerRoute and staffRoute are representative protected routes.
var (
	playerRoute = router.Route{Method: "*", Pattern: "/api/player/session/", Group: router.GroupPlayer}
	staffRoute  = router.Route{Method: "*", Pattern: "/api/admin/config/", Group: router.GroupStaff, MinRole: router.RoleLiveOps}
)

// assertRejected asserts the status, the COM-5 code and the metric reason agree.
func assertRejected(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantReason string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Errorf("status = %d, want %d (body %s)", w.Code, wantStatus, strings.TrimSpace(w.Body.String()))
	}
	if got := rejectionCode(t, w); got != wantReason {
		t.Errorf("COM-5 code = %q, want %q", got, wantReason)
	}
}
