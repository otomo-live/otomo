package token_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/auth/internal/token"
)

const (
	testKid      = "auth-2026-09-01"
	testIssuer   = "https://auth.otomo.internal"
	testAudience = "otomo:player"
	testSubject  = "3f1a2b4c-5d6e-4f70-8192-a3b4c5d6e7f8"

	// gatewaySkew is GATEWAY_JWT_CLOCK_SKEW's default and Session's
	// SESSION_JWT_CLOCK_SKEW default: the leeway every verifier applies to exp.
	gatewaySkew = 30 * time.Second
)

// gatewayParse verifies signed with exactly the options Gateway's authn middleware
// and Session's verifier pass to ParseWithClaims.
func gatewayParse(signed string, kf jwt.Keyfunc) (*jwt.Token, *token.Claims, error) {
	claims := &token.Claims{}
	parsed, err := jwt.ParseWithClaims(signed, claims, kf,
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer(testIssuer),
		jwt.WithAudience(testAudience),
		jwt.WithLeeway(gatewaySkew),
		jwt.WithExpirationRequired(),
	)
	return parsed, claims, err
}

func testSigner(priv ed25519.PrivateKey) *token.Signer {
	return &token.Signer{
		Kid:        testKid,
		PrivateKey: priv,
		Issuer:     testIssuer,
		Audience:   testAudience,
		TTL:        15 * time.Minute,
	}
}

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

func TestKeyFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing-key.pem")

	pub, priv := mustKey(t)
	if err := token.Write(path, priv); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode = %o, want 600", got)
		}
	}

	loaded, err := token.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Equal(priv) {
		t.Error("loaded private key differs from the one written")
	}
	if !token.Public(loaded).Equal(pub) {
		t.Error("derived public key differs from the generated one")
	}
}

func TestLoadRejectsUnusableFiles(t *testing.T) {
	dir := t.TempDir()

	notPEM := filepath.Join(dir, "not-pem.pem")
	if err := os.WriteFile(notPEM, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	wrongType := filepath.Join(dir, "wrong-type.pem")
	block := "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"
	if err := os.WriteFile(wrongType, []byte(block), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{notPEM, wrongType, filepath.Join(dir, "missing.pem")} {
		if _, err := token.Load(path); err == nil {
			t.Errorf("Load(%s) returned no error", filepath.Base(path))
		}
	}
}

func TestBuildJWKSMatchesTheContractShape(t *testing.T) {
	pubA, _ := mustKey(t)
	pubZ, _ := mustKey(t)

	raw, err := token.BuildJWKS([]token.PublicKey{
		{Kid: "z-key", Key: pubZ},
		{Kid: "a-key", Key: pubA},
	})
	if err != nil {
		t.Fatalf("BuildJWKS: %v", err)
	}

	var set token.JWKSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatalf("unmarshal JWKS: %v", err)
	}
	if len(set.Keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(set.Keys))
	}
	if set.Keys[0].Kid != "a-key" || set.Keys[1].Kid != "z-key" {
		t.Errorf("keys are not sorted by kid: %q, %q", set.Keys[0].Kid, set.Keys[1].Kid)
	}

	first := set.Keys[0]
	if first.Kty != "OKP" || first.Crv != "Ed25519" || first.Use != "sig" || first.Alg != "EdDSA" {
		t.Errorf("key attributes = %+v, want OKP/Ed25519/sig/EdDSA", first)
	}

	x, err := base64.RawURLEncoding.DecodeString(first.X)
	if err != nil {
		t.Fatalf("x is not base64url: %v", err)
	}
	if !ed25519.PublicKey(x).Equal(pubA) {
		t.Error("x does not decode to the public key that was passed in")
	}
}

func TestIssueVerifiesWithGatewaysParserOptions(t *testing.T) {
	pub, priv := mustKey(t)

	raw, err := token.BuildJWKS([]token.PublicKey{{Kid: testKid, Key: pub}})
	if err != nil {
		t.Fatalf("BuildJWKS: %v", err)
	}

	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer jwksServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	keySet, err := keyfunc.NewDefaultCtx(ctx, []string{jwksServer.URL})
	if err != nil {
		t.Fatalf("keyfunc.NewDefaultCtx: %v", err)
	}

	signed, err := testSigner(priv).Issue(testSubject, time.Now())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parsed, claims, err := gatewayParse(signed, keySet.Keyfunc)
	if err != nil {
		t.Fatalf("ParseWithClaims: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token did not validate")
	}
	if claims.Subject != testSubject {
		t.Errorf("sub = %q, want %s", claims.Subject, testSubject)
	}
	// Session's player verifier refuses any sub that does not parse as a UUID.
	if _, err := uuid.Parse(claims.Subject); err != nil {
		t.Errorf("sub %q would be refused by Session: %v", claims.Subject, err)
	}
	if got := parsed.Header["kid"]; got != testKid {
		t.Errorf("header kid = %v, want %s", got, testKid)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != testAudience {
		t.Errorf("aud = %v, want the single-element array [%s]", claims.Audience, testAudience)
	}
	if claims.ExpiresAt == nil || claims.NotBefore == nil || claims.IssuedAt == nil {
		t.Error("exp, nbf and iat must all be present")
	}

	parts := strings.Split(signed, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if strings.Contains(string(payload), "roles") {
		t.Errorf("player tokens must not carry a roles claim: %s", payload)
	}
}

func TestIssueRejectsAWrongAudience(t *testing.T) {
	pub, priv := mustKey(t)

	raw, err := token.BuildJWKS([]token.PublicKey{{Kid: testKid, Key: pub}})
	if err != nil {
		t.Fatalf("BuildJWKS: %v", err)
	}

	keySet, err := keyfunc.NewJWKSetJSON(raw)
	if err != nil {
		t.Fatalf("keyfunc.NewJWKSetJSON: %v", err)
	}

	signed, err := testSigner(priv).Issue(testSubject, time.Now())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	_, err = jwt.ParseWithClaims(signed, &token.Claims{}, keySet.Keyfunc,
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer(testIssuer),
		jwt.WithAudience("otomo:staff"),
		jwt.WithExpirationRequired(),
	)
	if err == nil {
		t.Fatal("a player token was accepted for the staff audience")
	}
}

func TestIssueRejectsAnEmptyOrNonUUIDSubject(t *testing.T) {
	_, priv := mustKey(t)
	signer := testSigner(priv)

	for _, sub := range []string{
		"",
		"player-123",
		"3F1A2B4C-5D6E-4F70-8192-A3B4C5D6E7F8",   // upper case: a second spelling of one account
		"{3f1a2b4c-5d6e-4f70-8192-a3b4c5d6e7f8}", // braced
		"urn:uuid:3f1a2b4c-5d6e-4f70-8192-a3b4c5d6e7f8", // URN
		"3f1a2b4c5d6e4f708192a3b4c5d6e7f8",              // no hyphens
		"00000000-0000-0000-0000-000000000000",          // nil: an unset account_id
	} {
		signed, err := signer.Issue(sub, time.Now())
		if !errors.Is(err, token.ErrInvalidSubject) {
			t.Errorf("Issue(%q) = %q, %v; want ErrInvalidSubject", sub, signed, err)
		}
		if signed != "" {
			t.Errorf("Issue(%q) returned a token alongside the error", sub)
		}
	}
}

func TestExpiredTokenIsAcceptedOnlyWithinTheLeeway(t *testing.T) {
	pub, priv := mustKey(t)
	raw, err := token.BuildJWKS([]token.PublicKey{{Kid: testKid, Key: pub}})
	if err != nil {
		t.Fatalf("BuildJWKS: %v", err)
	}
	keySet, err := keyfunc.NewJWKSetJSON(raw)
	if err != nil {
		t.Fatalf("keyfunc.NewJWKSetJSON: %v", err)
	}
	signer := testSigner(priv)

	// Issued so that exp is 10 s in the past: inside the 30 s leeway, so Gateway and
	// Session both still accept it. This is the window that absorbs clock skew between
	// Auth and a verifier, and it is why a client can see a 401 expired up to 30 s after
	// expires_in says the token ran out.
	within, err := signer.Issue(testSubject, time.Now().Add(-signer.TTL-10*time.Second))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, _, err := gatewayParse(within, keySet.Keyfunc); err != nil {
		t.Errorf("token 10s past exp was rejected inside the %v leeway: %v", gatewaySkew, err)
	}

	// exp 40 s in the past: beyond the leeway (with margin for NumericDate's truncation
	// to whole seconds), so it must fail as expired and nothing else — expired is the
	// code that tells the client to refresh.
	beyond, err := signer.Issue(testSubject, time.Now().Add(-signer.TTL-40*time.Second))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, _, err = gatewayParse(beyond, keySet.Keyfunc)
	if !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("token 40s past exp: err = %v, want ErrTokenExpired", err)
	}
	if errors.Is(err, jwt.ErrTokenSignatureInvalid) || errors.Is(err, jwt.ErrTokenInvalidIssuer) ||
		errors.Is(err, jwt.ErrTokenInvalidAudience) {
		t.Errorf("an expired but otherwise valid token also reported another failure: %v", err)
	}
}
