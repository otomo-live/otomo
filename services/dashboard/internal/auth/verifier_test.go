package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testKid      = "test-staff-key"
	testIssuer   = "https://php-admin.otomo.internal"
	testAudience = "otomo:staff"
)

// jwks serves a one-key JWKS document for pub. The key is the only one the verifier
// will ever accept, so every rejection test is really "this token is not one this key
// vouches for".
func jwks(t *testing.T, pub ed25519.PublicKey) *httptest.Server {
	t.Helper()

	doc := map[string]any{"keys": []map[string]string{{
		"kty": "OKP",
		"crv": "Ed25519",
		"kid": testKid,
		"x":   base64.RawURLEncoding.EncodeToString(pub),
		"alg": "EdDSA",
		"use": "sig",
	}}}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newVerifier returns a started verifier pointed at a fresh key pair's JWKS, waited
// until it has fetched the key. Waiting here rather than in each test is what makes
// the rejection tests mean something: they must fail because of the token, not
// because the verifier had not loaded any key yet.
func newVerifier(t *testing.T) (*Verifier, ed25519.PrivateKey) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	srv := jwks(t, pub)
	v := NewVerifier(srv.URL, testIssuer, testAudience, time.Minute, 0)
	v.Start(t.Context())

	deadline := time.Now().Add(5 * time.Second)
	for !v.Ready(t.Context()) {
		if time.Now().After(deadline) {
			t.Fatal("verifier never fetched the jwks")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return v, priv
}

// sign mints a staff token. mut lets a test corrupt exactly one claim, so a failure
// names which claim the verifier objected to.
func sign(t *testing.T, priv ed25519.PrivateKey, mut func(jwt.MapClaims)) string {
	t.Helper()

	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   testIssuer,
		"aud":   testAudience,
		"sub":   "staff-1",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"roles": []string{"viewer"},
	}
	if mut != nil {
		mut(claims)
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = testKid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestVerifyAcceptsAWellFormedStaffToken(t *testing.T) {
	v, priv := newVerifier(t)

	claims, reason := v.Verify(t.Context(), sign(t, priv, nil))
	if reason != "" {
		t.Fatalf("Verify() rejected a valid token with reason %q", reason)
	}
	if claims.Subject != "staff-1" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "staff-1")
	}
	if !claims.HasRoleAtLeast(RoleViewer) {
		t.Error("claims do not carry the viewer role the token was signed with")
	}
}

// TestVerifyRejections is the table that matters: each row is one way a token can be
// wrong, and the expected reason is what an operator will see in
// dashboard_token_rejected_total. A row that produced the wrong reason would still
// reject the request, which is why asserting only the rejection would not be enough.
func TestVerifyRejections(t *testing.T) {
	v, priv := newVerifier(t)

	tests := []struct {
		name   string
		token  func(t *testing.T) string
		reason string
	}{
		{
			name:   "empty",
			token:  func(*testing.T) string { return "" },
			reason: ReasonMissingToken,
		},
		{
			name:   "not a jwt",
			token:  func(*testing.T) string { return "not-a-token" },
			reason: ReasonInvalidToken,
		},
		{
			name: "signed by another key",
			token: func(t *testing.T) string {
				_, other, err := ed25519.GenerateKey(nil)
				if err != nil {
					t.Fatalf("generate key: %v", err)
				}
				return sign(t, other, nil)
			},
			reason: ReasonInvalidSignature,
		},
		{
			name: "expired",
			token: func(t *testing.T) string {
				return sign(t, priv, func(c jwt.MapClaims) {
					c["exp"] = time.Now().Add(-2 * time.Hour).Unix()
					c["iat"] = time.Now().Add(-3 * time.Hour).Unix()
				})
			},
			reason: ReasonExpired,
		},
		{
			name: "no exp",
			token: func(t *testing.T) string {
				return sign(t, priv, func(c jwt.MapClaims) { delete(c, "exp") })
			},
			reason: ReasonInvalidToken,
		},
		{
			name: "another issuer",
			token: func(t *testing.T) string {
				return sign(t, priv, func(c jwt.MapClaims) { c["iss"] = "https://auth.otomo.internal" })
			},
			reason: ReasonIssuerMismatch,
		},
		{
			// The case COM-4 exists for: a player token is correctly signed but was
			// never meant for this service.
			name: "player audience",
			token: func(t *testing.T) string {
				return sign(t, priv, func(c jwt.MapClaims) { c["aud"] = "otomo:player" })
			},
			reason: ReasonAudienceMismatch,
		},
		{
			name: "no subject",
			token: func(t *testing.T) string {
				return sign(t, priv, func(c jwt.MapClaims) { delete(c, "sub") })
			},
			reason: ReasonInvalidToken,
		},
		{
			name: "not yet valid",
			token: func(t *testing.T) string {
				return sign(t, priv, func(c jwt.MapClaims) {
					c["nbf"] = time.Now().Add(time.Hour).Unix()
				})
			},
			reason: ReasonInvalidToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, reason := v.Verify(t.Context(), tt.token(t))
			if reason != tt.reason {
				t.Fatalf("reason = %q, want %q", reason, tt.reason)
			}
			if claims != nil {
				t.Errorf("claims returned with a rejection: %+v", claims)
			}
			if MessageFor(reason) == "" {
				t.Error("MessageFor returned an empty message for a reason the service can produce")
			}
		})
	}
}

// TestVerifyRejectsAnUnsignedToken covers algorithm substitution from the other side
// from the signature test: a token whose header asks for "none" is well-formed and
// carries the right claims, so nothing but the pinned algorithm list stops it.
func TestVerifyRejectsAnUnsignedToken(t *testing.T) {
	v, _ := newVerifier(t)

	tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": "staff-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = testKid
	unsigned, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("build unsigned token: %v", err)
	}

	if _, reason := v.Verify(t.Context(), unsigned); reason != ReasonInvalidSignature {
		t.Fatalf("reason = %q, want %q", reason, ReasonInvalidSignature)
	}
}

// TestVerifyRejectsEverythingBeforeTheFirstFetch pins the fail-closed direction: a
// verifier whose JWKS has not answered yet cannot admit anyone, including the token it
// would accept a moment later.
func TestVerifyRejectsEverythingBeforeTheFirstFetch(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv := jwks(t, pub)

	v := NewVerifier(srv.URL, testIssuer, testAudience, time.Minute, 0)
	// Deliberately not started.

	token := sign(t, priv, nil)
	if _, reason := v.Verify(t.Context(), token); reason != ReasonInvalidToken {
		t.Fatalf("reason = %q, want %q", reason, ReasonInvalidToken)
	}
	if v.Ready(t.Context()) {
		t.Error("Ready reported true before any key was fetched")
	}
}

// TestUnavailableJwksDoesNotBlockServing is the property main depends on: PHP Admin
// Auth being undeployed must not stop this service from starting or from answering
// requests. The URL is in TEST-NET-1 (RFC 5737), so the fetch hangs until its own
// timeout; neither Start nor Verify may wait for it.
func TestUnavailableJwksDoesNotBlockServing(t *testing.T) {
	// A closed listener would fail instantly; this address never answers at all.
	v := NewVerifier("http://192.0.2.1/.well-known/jwks.json", testIssuer, testAudience, time.Minute, 0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		v.Start(t.Context())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked on an unreachable jwks")
	}

	if v.Ready(t.Context()) {
		t.Error("Ready reported true with no reachable jwks")
	}
	done = make(chan struct{})
	go func() {
		defer close(done)
		if _, reason := v.Verify(t.Context(), "anything"); reason != ReasonInvalidToken {
			t.Errorf("reason = %q, want %q", reason, ReasonInvalidToken)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Verify blocked on an unreachable jwks")
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"", ""},
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER abc", "abc"},
		{"Bearer  abc ", "abc"},
		{"Basic abc", ""},
		{"Bear", ""},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tt.header != "" {
			r.Header.Set("Authorization", tt.header)
		}
		if got := BearerToken(r); got != tt.want {
			t.Errorf("BearerToken(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

// TestReasonForOrdering pins the two ordering decisions in ReasonFor. Both are
// load-bearing and neither is observable in a passing request: a token that is forged
// *and* expired must be labelled by its signature, because reporting "expired" would
// send an operator to look at clocks instead of at who is forging tokens; and a
// token from another domain is wrong on issuer and audience at once, so the issuer —
// the claim that names which domain it came from — wins.
func TestReasonForOrdering(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ReasonInvalidToken},
		{"expired", jwt.ErrTokenExpired, ReasonExpired},
		{"issuer", jwt.ErrTokenInvalidIssuer, ReasonIssuerMismatch},
		{"audience", jwt.ErrTokenInvalidAudience, ReasonAudienceMismatch},
		{"signature", jwt.ErrTokenSignatureInvalid, ReasonInvalidSignature},
		{"unverifiable", jwt.ErrTokenUnverifiable, ReasonInvalidSignature},
		{"unrelated", context.DeadlineExceeded, ReasonInvalidToken},
		{
			// The ordering the comments claim, exercised rather than described.
			"forged and stale",
			errors.Join(jwt.ErrTokenSignatureInvalid, jwt.ErrTokenExpired),
			ReasonInvalidSignature,
		},
		{
			"cross-domain",
			fmt.Errorf("%w: %w", jwt.ErrTokenInvalidAudience, jwt.ErrTokenInvalidIssuer),
			ReasonIssuerMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReasonFor(tt.err); got != tt.want {
				t.Errorf("ReasonFor(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestParseRole(t *testing.T) {
	for _, name := range []string{"viewer", "live_ops", "admin"} {
		if _, ok := ParseRole(name); !ok {
			t.Errorf("ParseRole(%q) reported unknown", name)
		}
	}
	if r, ok := ParseRole("root"); ok {
		t.Errorf("ParseRole(root) = %v, true; want unknown", r)
	}
}

func TestHasRoleAtLeast(t *testing.T) {
	tests := []struct {
		roles []string
		min   Role
		want  bool
	}{
		{nil, RoleViewer, false},
		{[]string{"viewer"}, RoleViewer, true},
		{[]string{"viewer"}, RoleLiveOps, false},
		{[]string{"live_ops"}, RoleViewer, true},
		{[]string{"live_ops"}, RoleAdmin, false},
		{[]string{"admin"}, RoleAdmin, true},
		{[]string{"viewer", "live_ops"}, RoleLiveOps, true},
		{[]string{"viewer", "live_ops"}, RoleAdmin, false},
		// An unrecognised role grants nothing rather than failing the token: a
		// hierarchy that grew a name this build does not know must not lock that
		// staff member out of the roles the build does know.
		{[]string{"root"}, RoleViewer, false},
		{[]string{"root", "admin"}, RoleAdmin, true},
	}
	for _, tt := range tests {
		c := &Claims{Roles: tt.roles}
		if got := c.HasRoleAtLeast(tt.min); got != tt.want {
			t.Errorf("HasRoleAtLeast(%v, %s) = %v, want %v", tt.roles, tt.min, got, tt.want)
		}
	}
}

func TestClaimsRoundTripThroughContext(t *testing.T) {
	if ClaimsFrom(t.Context()) != nil {
		t.Error("ClaimsFrom returned claims for a context that never carried any")
	}
	want := &Claims{Roles: []string{"admin"}}
	if got := ClaimsFrom(WithClaims(t.Context(), want)); got != want {
		t.Errorf("ClaimsFrom = %p, want %p", got, want)
	}
}

// TestTokenRoundTripThroughContext keeps the audit handlers' source of the caller's raw
// credential honest: absent means absent, and present comes back verbatim.
func TestTokenRoundTripThroughContext(t *testing.T) {
	if got := TokenFrom(t.Context()); got != "" {
		t.Errorf("TokenFrom returned %q for a context that never carried one", got)
	}
	if got := TokenFrom(WithToken(t.Context(), "abc.def.ghi")); got != "abc.def.ghi" {
		t.Errorf("TokenFrom = %q, want abc.def.ghi", got)
	}
}

func TestRoleString(t *testing.T) {
	tests := []struct {
		role Role
		want string
	}{
		{RoleViewer, "viewer"},
		{RoleLiveOps, "live_ops"},
		{RoleAdmin, "admin"},
		{Role(0), "none"},
		// A value no constant names, so the fallback is covered rather than assumed.
		{Role(42), "none"},
	}
	for _, tt := range tests {
		if got := tt.role.String(); got != tt.want {
			t.Errorf("Role(%d).String() = %q, want %q", tt.role, got, tt.want)
		}
	}
}
