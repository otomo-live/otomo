package auth

import (
	"bytes"
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
	"uuid"
)

// The two domains, as a deployment configures them. They differ in every dimension the
// verifier checks — key, issuer and audience — which is what lets these tests assert that
// a token from one is refused by the other for a reason an operator can act on.
const (
	testKid = "test-key"

	playerIssuer   = "https://auth.otomo.internal"
	playerAudience = "otomo:player"

	staffIssuer   = "https://php-admin.otomo.internal"
	staffAudience = "otomo:staff"
)

// playerID is a subject the player verifier accepts: a UUID, because that is
// player_profile's primary key and the query it feeds.
const playerID = "018f4a3e-1c2d-7abc-8def-0123456789ab"

// jwks serves a one-key JWKS document for pub. The key is the only one the verifier will
// ever accept, so every rejection test is really "this token is not one this key
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

// newVerifier returns a started verifier of the given domain, pointed at a fresh key
// pair's JWKS and waited until it has fetched the key. Waiting here rather than in each
// test is what makes the rejection tests mean something: they must fail because of the
// token, not because the verifier had not loaded any key yet.
func newVerifier(t *testing.T, domain Domain) (*Verifier, ed25519.PrivateKey) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return verifierAt(t, domain, jwks(t, pub).URL), priv
}

// atURL returns a started verifier of domain pointed at an existing JWKS endpoint, for
// the tests that need both domains to read *one* key set — which is the misconfiguration
// the two-issuer check exists to survive.
func atURL(t *testing.T, domain Domain, url string) *Verifier {
	t.Helper()
	return verifierAt(t, domain, url)
}

func verifierAt(t *testing.T, domain Domain, url string) *Verifier {
	t.Helper()

	var v *Verifier
	switch domain {
	case DomainPlayer:
		v = NewPlayerVerifier(url, playerIssuer, playerAudience, time.Minute, 0)
	default:
		v = NewStaffVerifier(url, staffIssuer, staffAudience, time.Minute, 0)
	}
	v.Start(t.Context())

	deadline := time.Now().Add(5 * time.Second)
	for !v.Ready(t.Context()) {
		if time.Now().After(deadline) {
			t.Fatal("verifier never fetched the jwks")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return v
}

// claims returns the starting claim set for one domain. mut lets a test corrupt exactly
// one claim, so a failure names which claim the verifier objected to.
func claims(domain Domain) jwt.MapClaims {
	now := time.Now()
	c := jwt.MapClaims{
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	if domain == DomainPlayer {
		c["iss"] = playerIssuer
		c["aud"] = playerAudience
		c["sub"] = playerID
		return c
	}
	c["iss"] = staffIssuer
	c["aud"] = staffAudience
	c["sub"] = "staff-1"
	c["roles"] = []string{"viewer"}
	return c
}

// sign mints a token for one domain, optionally mutating its claims first.
func sign(t *testing.T, domain Domain, priv ed25519.PrivateKey, mut func(jwt.MapClaims)) string {
	t.Helper()

	c := claims(domain)
	if mut != nil {
		mut(c)
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	tok.Header["kid"] = testKid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestVerifyAcceptsAWellFormedPlayerToken(t *testing.T) {
	v, priv := newVerifier(t, DomainPlayer)

	id, reason := v.Verify(t.Context(), sign(t, DomainPlayer, priv, nil))
	if reason != "" {
		t.Fatalf("Verify() rejected a valid token with reason %q", reason)
	}
	if id.Subject != playerID {
		t.Errorf("Subject = %q, want %q", id.Subject, playerID)
	}
	if got := id.PlayerUUID().String(); got != playerID {
		t.Errorf("PlayerUUID() = %q, want %q", got, playerID)
	}
}

func TestVerifyAcceptsAWellFormedStaffToken(t *testing.T) {
	v, priv := newVerifier(t, DomainStaff)

	id, reason := v.Verify(t.Context(), sign(t, DomainStaff, priv, nil))
	if reason != "" {
		t.Fatalf("Verify() rejected a valid token with reason %q", reason)
	}
	if id.Subject != "staff-1" {
		t.Errorf("Subject = %q, want %q", id.Subject, "staff-1")
	}
	if !id.HasRoleAtLeast(RoleViewer) {
		t.Error("identity does not carry the viewer role the token was signed with")
	}
}

// TestPlayerTokenCannotCarryAStaffRole is the second half of SES-A1's boundary, and it
// is not reachable by configuration: the token below is correctly signed by the player
// domain's own key, carries the player issuer and audience, and still asks for `admin`.
// The only thing refusing it is that the player domain ignores the claim — which is what
// keeps a player out of the admin surface even if Auth is ever made to mint roles, or if
// a future change ever points both verifiers at one key.
func TestPlayerTokenCannotCarryAStaffRole(t *testing.T) {
	v, priv := newVerifier(t, DomainPlayer)

	id, reason := v.Verify(t.Context(), sign(t, DomainPlayer, priv, func(c jwt.MapClaims) {
		c["roles"] = []string{"admin", "live_ops", "viewer"}
	}))
	if reason != "" {
		t.Fatalf("Verify() rejected the token with reason %q; it is otherwise valid", reason)
	}
	if len(id.Roles) != 0 {
		t.Fatalf("player identity carries roles %v; the player domain must ignore that claim", id.Roles)
	}
	if id.HasRoleAtLeast(RoleViewer) {
		t.Error("a player token satisfies a staff route's minimum role")
	}
}

// TestDomainsDoNotCross is SES-A1's acceptance criterion, checked on the verifiers
// directly: each domain's token is refused by the other.
//
// The reason matters as much as the refusal, and it is *not* what one would guess. In a
// real deployment the two domains publish to two JWKS endpoints with two different keys,
// so a cross-domain token is refused earlier than any claim check: the other domain's
// `kid` is simply not in this key set, which jwt reports as an unverifiable token.
// invalid_signature is therefore the code an operator will actually see for a
// cross-domain call, and it is the honest one — nothing about that token was verified
// here, not even its issuer, because the issuer is only meaningful on a token whose
// signature holds.
func TestDomainsDoNotCross(t *testing.T) {
	player, playerKey := newVerifier(t, DomainPlayer)
	staff, staffKey := newVerifier(t, DomainStaff)

	if bytes.Equal(playerKey.Public().(ed25519.PublicKey), staffKey.Public().(ed25519.PublicKey)) {
		t.Fatal("the two domains were built on the same key; this test would pass for the wrong reason")
	}

	t.Run("staff token on the player verifier", func(t *testing.T) {
		if _, reason := player.Verify(t.Context(), sign(t, DomainStaff, staffKey, nil)); reason != ReasonInvalidSignature {
			t.Fatalf("reason = %q, want %q", reason, ReasonInvalidSignature)
		}
	})
	t.Run("player token on the staff verifier", func(t *testing.T) {
		if _, reason := staff.Verify(t.Context(), sign(t, DomainPlayer, playerKey, nil)); reason != ReasonInvalidSignature {
			t.Fatalf("reason = %q, want %q", reason, ReasonInvalidSignature)
		}
	})
}

// TestSharedKeyStillSeparatesTheDomains is the case the key check alone would not catch:
// both verifiers reading *one* JWKS, which is what a copy-pasted environment block
// produces. The keys now match, so every signature verifies, and the only thing left
// holding the domains apart is the claim check — iss before aud, per ReasonFor.
//
// This is the test that proves the iss/aud checks are load-bearing rather than
// belt-and-braces, and it is deliberately not the deployment's shape.
func TestSharedKeyStillSeparatesTheDomains(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	url := jwks(t, pub).URL

	player := atURL(t, DomainPlayer, url)
	staff := atURL(t, DomainStaff, url)

	t.Run("a staff token presented as a player token", func(t *testing.T) {
		if _, reason := player.Verify(t.Context(), sign(t, DomainStaff, priv, nil)); reason != ReasonIssuerMismatch {
			t.Fatalf("reason = %q, want %q", reason, ReasonIssuerMismatch)
		}
	})
	t.Run("a player token presented as a staff token", func(t *testing.T) {
		if _, reason := staff.Verify(t.Context(), sign(t, DomainPlayer, priv, nil)); reason != ReasonIssuerMismatch {
			t.Fatalf("reason = %q, want %q", reason, ReasonIssuerMismatch)
		}
	})
	t.Run("issuer right, audience wrong", func(t *testing.T) {
		// The player issuer with the staff audience: the issuer check passes and the
		// audience check is the one that has to refuse it.
		if _, reason := player.Verify(t.Context(), sign(t, DomainPlayer, priv, func(c jwt.MapClaims) {
			c["aud"] = staffAudience
		})); reason != ReasonAudienceMismatch {
			t.Fatalf("reason = %q, want %q", reason, ReasonAudienceMismatch)
		}
	})
}

// TestVerifyRejections is the table that matters: each row is one way a token can be
// wrong, and the expected reason is what an operator will see in
// session_token_rejected_total. A row that produced the wrong reason would still reject
// the request, which is why asserting only the rejection would not be enough.
func TestVerifyRejections(t *testing.T) {
	v, priv := newVerifier(t, DomainPlayer)

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
				return sign(t, DomainPlayer, other, nil)
			},
			reason: ReasonInvalidSignature,
		},
		{
			name: "expired",
			token: func(t *testing.T) string {
				return sign(t, DomainPlayer, priv, func(c jwt.MapClaims) {
					c["exp"] = time.Now().Add(-2 * time.Hour).Unix()
					c["iat"] = time.Now().Add(-3 * time.Hour).Unix()
				})
			},
			reason: ReasonExpired,
		},
		{
			name: "no exp",
			token: func(t *testing.T) string {
				return sign(t, DomainPlayer, priv, func(c jwt.MapClaims) { delete(c, "exp") })
			},
			reason: ReasonInvalidToken,
		},
		{
			name: "another issuer",
			token: func(t *testing.T) string {
				return sign(t, DomainPlayer, priv, func(c jwt.MapClaims) { c["iss"] = staffIssuer })
			},
			reason: ReasonIssuerMismatch,
		},
		{
			name: "another audience",
			token: func(t *testing.T) string {
				return sign(t, DomainPlayer, priv, func(c jwt.MapClaims) { c["aud"] = staffAudience })
			},
			reason: ReasonAudienceMismatch,
		},
		{
			name: "no subject",
			token: func(t *testing.T) string {
				return sign(t, DomainPlayer, priv, func(c jwt.MapClaims) { delete(c, "sub") })
			},
			reason: ReasonInvalidToken,
		},
		{
			// A player subject is a UUID because player_profile's primary key is one.
			// Refusing it here is what turns "the database would have rejected this
			// cast" into a 401 the caller can understand.
			name: "subject is not a uuid",
			token: func(t *testing.T) string {
				return sign(t, DomainPlayer, priv, func(c jwt.MapClaims) { c["sub"] = "player-1" })
			},
			reason: ReasonInvalidToken,
		},
		{
			name: "not yet valid",
			token: func(t *testing.T) string {
				return sign(t, DomainPlayer, priv, func(c jwt.MapClaims) {
					c["nbf"] = time.Now().Add(time.Hour).Unix()
				})
			},
			reason: ReasonInvalidToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, reason := v.Verify(t.Context(), tt.token(t))
			if reason != tt.reason {
				t.Fatalf("reason = %q, want %q", reason, tt.reason)
			}
			if id != nil {
				t.Errorf("identity returned with a rejection: %+v", id)
			}
			if MessageFor(reason) == "" {
				t.Error("MessageFor returned an empty message for a reason the service can produce")
			}
		})
	}
}

// TestStaffSubjectIsNotRequiredToBeAUUID is the asymmetry between the domains: `sub` is
// PHP's own staff account id, which this service does not own and must not assume is a
// uuid. A staff token with an account id like "staff-1" is the normal case, not an error.
func TestStaffSubjectIsNotRequiredToBeAUUID(t *testing.T) {
	v, priv := newVerifier(t, DomainStaff)

	id, reason := v.Verify(t.Context(), sign(t, DomainStaff, priv, nil))
	if reason != "" {
		t.Fatalf("Verify() rejected a staff token with a non-uuid subject: %q", reason)
	}
	if id.Subject != "staff-1" {
		t.Errorf("Subject = %q, want %q", id.Subject, "staff-1")
	}
}

// TestVerifyRejectsAnUnsignedToken covers algorithm substitution from the other side from
// the signature test: a token whose header asks for "none" is well-formed and carries the
// right claims, so nothing but the pinned algorithm list stops it.
func TestVerifyRejectsAnUnsignedToken(t *testing.T) {
	v, _ := newVerifier(t, DomainPlayer)

	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims(DomainPlayer))
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

	v := NewPlayerVerifier(srv.URL, playerIssuer, playerAudience, time.Minute, 0)
	// Deliberately not started.

	token := sign(t, DomainPlayer, priv, nil)
	if _, reason := v.Verify(t.Context(), token); reason != ReasonInvalidToken {
		t.Fatalf("reason = %q, want %q", reason, ReasonInvalidToken)
	}
	if v.Ready(t.Context()) {
		t.Error("Ready reported true before any key was fetched")
	}
}

// TestUnavailableJwksDoesNotBlockServing is the property main depends on: an issuer being
// undeployed must not stop this service from starting or from answering requests. The URL
// is in TEST-NET-1 (RFC 5737), so the fetch hangs until its own timeout; neither Start nor
// Verify may wait for it.
func TestUnavailableJwksDoesNotBlockServing(t *testing.T) {
	// A closed listener would fail instantly; this address never answers at all.
	v := NewPlayerVerifier("http://192.0.2.1/.well-known/jwks.json", playerIssuer, playerAudience, time.Minute, 0)

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

func TestDomainAccessors(t *testing.T) {
	player, _ := newVerifier(t, DomainPlayer)
	staff, _ := newVerifier(t, DomainStaff)

	if player.Domain() != DomainPlayer {
		t.Errorf("player verifier Domain() = %q", player.Domain())
	}
	if staff.Domain() != DomainStaff {
		t.Errorf("staff verifier Domain() = %q", staff.Domain())
	}
	if player.URL() == "" || staff.URL() == "" {
		t.Error("URL() is empty; a readiness message would not name the endpoint")
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
// *and* expired must be labelled by its signature, because reporting "expired" would send
// an operator to look at clocks instead of at who is forging tokens; and a token from the
// other domain is wrong on issuer and audience at once, so the issuer — the claim that
// names which domain it came from — wins.
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
		// An unrecognised role grants nothing rather than failing the token: a hierarchy
		// that grew a name this build does not know must not lock that staff member out
		// of the roles the build does know.
		{[]string{"root"}, RoleViewer, false},
		{[]string{"root", "admin"}, RoleAdmin, true},
		// A player route's minimum, which every identity satisfies — including one that
		// carries no roles at all, which is every player token.
		{nil, RoleNone, true},
	}
	for _, tt := range tests {
		id := &Identity{Roles: tt.roles}
		if got := id.HasRoleAtLeast(tt.min); got != tt.want {
			t.Errorf("HasRoleAtLeast(%v, %s) = %v, want %v", tt.roles, tt.min, got, tt.want)
		}
	}
}

func TestIdentityRoundTripThroughContext(t *testing.T) {
	if IdentityFrom(t.Context()) != nil {
		t.Error("IdentityFrom returned an identity for a context that never carried one")
	}
	want := &Identity{Subject: playerID, Roles: []string{"admin"}}
	if got := IdentityFrom(WithIdentity(t.Context(), want)); got != want {
		t.Errorf("IdentityFrom = %p, want %p", got, want)
	}
}

// TestPlayerUUIDOfAHandBuiltIdentity pins the fallback: a zero UUID rather than a panic.
// It is unreachable through Verify, so the only way to it is a bug in this service, and a
// query that matches nothing is a better answer to a bug than a crash in a request.
func TestPlayerUUIDOfAHandBuiltIdentity(t *testing.T) {
	if got := (&Identity{Subject: "not-a-uuid"}).PlayerUUID(); got != (uuid.UUID{}) {
		t.Errorf("PlayerUUID() = %v, want the zero UUID", got)
	}
}

func TestRoleString(t *testing.T) {
	tests := []struct {
		role Role
		want string
	}{
		{RoleNone, "none"},
		{RoleViewer, "viewer"},
		{RoleLiveOps, "live_ops"},
		{RoleAdmin, "admin"},
		// A value no constant names, so the fallback is covered rather than assumed.
		{Role(42), "none"},
	}
	for _, tt := range tests {
		if got := tt.role.String(); got != tt.want {
			t.Errorf("Role(%d).String() = %q, want %q", tt.role, got, tt.want)
		}
	}
}

// TestStaffNameIsKeptForAuditRows: the staff token's name claim reaches the identity,
// and a player token's never does (SE-7).
func TestStaffNameIsKeptForAuditRows(t *testing.T) {
	v, priv := newVerifier(t, DomainStaff)
	id, reason := v.Verify(t.Context(), sign(t, DomainStaff, priv, func(c jwt.MapClaims) { c["name"] = "Grace Hopper" }))
	if reason != "" || id.Name != "Grace Hopper" || id.ActorName() != "Grace Hopper" {
		t.Errorf("staff name = %q (%q), actor %q", id.Name, reason, id.ActorName())
	}
	nameless, _ := v.Verify(t.Context(), sign(t, DomainStaff, priv, nil))
	if nameless.ActorName() != "staff-1" {
		t.Errorf("actor without a name = %q, want the subject", nameless.ActorName())
	}

	pv, ppriv := newVerifier(t, DomainPlayer)
	pid, _ := pv.Verify(t.Context(), sign(t, DomainPlayer, ppriv, func(c jwt.MapClaims) { c["name"] = "Mallory" }))
	if pid.Name != "" {
		t.Errorf("a player token's name reached the identity: %q", pid.Name)
	}
}
