package token_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

const (
	testKid      = "admin-auth-2026-09-01"
	testIssuer   = "https://admin-auth.otomo.internal"
	testAudience = "otomo:staff"
)

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

// publicKeyFunc answers jwt with the one public key under test, standing in for the
// gateway's JWKS-backed keyfunc without pulling in that dependency.
func publicKeyFunc(pub ed25519.PublicKey) jwt.Keyfunc {
	return func(*jwt.Token) (any, error) { return pub, nil }
}

func TestIssueVerifiesWithTheGatewaysParserOptions(t *testing.T) {
	pub, priv := mustKey(t)

	signer := &token.Signer{
		Kid:        testKid,
		PrivateKey: priv,
		Issuer:     testIssuer,
		Audience:   testAudience,
		TTL:        15 * time.Minute,
	}
	now := time.Now()
	signed, err := signer.Issue("staff-123", "Ada Lovelace", []string{"admin", "viewer"}, now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	claims := &token.Claims{}
	parsed, err := jwt.ParseWithClaims(signed, claims, publicKeyFunc(pub),
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer(testIssuer),
		jwt.WithAudience(testAudience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		t.Fatalf("ParseWithClaims: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token did not validate")
	}
	if claims.Subject != "staff-123" {
		t.Errorf("sub = %q, want staff-123", claims.Subject)
	}
	if got := parsed.Header["kid"]; got != testKid {
		t.Errorf("header kid = %v, want %s", got, testKid)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != testAudience {
		t.Errorf("aud = %v, want the single-element array [%s]", claims.Audience, testAudience)
	}
	if got, want := claims.Roles, []string{"admin", "viewer"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roles = %v, want %v", got, want)
	}
	if claims.Name != "Ada Lovelace" {
		t.Errorf("name = %q, want Ada Lovelace", claims.Name)
	}
	if claims.ExpiresAt == nil || claims.NotBefore == nil || claims.IssuedAt == nil {
		t.Fatal("exp, nbf and iat must all be present")
	}
	if got := claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time); got != 15*time.Minute {
		t.Errorf("exp - iat = %v, want the 15m TTL", got)
	}
}

func TestIssueRejectsEmptyRoles(t *testing.T) {
	_, priv := mustKey(t)

	signer := &token.Signer{
		Kid:        testKid,
		PrivateKey: priv,
		Issuer:     testIssuer,
		Audience:   testAudience,
		TTL:        15 * time.Minute,
	}
	if _, err := signer.Issue("staff-123", "Ada Lovelace", nil, time.Now()); err == nil {
		t.Fatal("Issue with no roles returned no error")
	}
}
