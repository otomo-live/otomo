package token_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// newVerifier builds a Verifier over one test key, plus a signer for it and the
// PublicKey set the verifier was built from.
func newVerifier(t *testing.T) (*token.Verifier, *token.Signer, []token.PublicKey) {
	t.Helper()

	pub, priv := mustKey(t)
	keys := []token.PublicKey{{Kid: testKid, Key: pub}}
	v := token.NewVerifier(testIssuer, testAudience, keys)
	signer := &token.Signer{
		Kid:        testKid,
		PrivateKey: priv,
		Issuer:     testIssuer,
		Audience:   testAudience,
		TTL:        15 * time.Minute,
	}
	return v, signer, keys
}

func TestVerifierAcceptsItsOwnToken(t *testing.T) {
	v, signer, _ := newVerifier(t)

	signed, err := signer.Issue("staff-1", "Ada", []string{"admin"}, time.Now())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	claims, reason := v.Verify(signed)
	if reason != "" {
		t.Fatalf("Verify reason = %q, want accepted", reason)
	}
	if claims.Subject != "staff-1" {
		t.Errorf("sub = %q, want staff-1", claims.Subject)
	}
}

func TestVerifierRejections(t *testing.T) {
	v, signer, keys := newVerifier(t)
	now := time.Now()

	valid, err := signer.Issue("staff-1", "Ada", []string{"admin"}, now)
	if err != nil {
		t.Fatalf("Issue valid: %v", err)
	}
	tampered := tamperSignature(t, valid)

	expiredSigner := *signer
	expiredSigner.TTL = -time.Hour
	expired, err := expiredSigner.Issue("staff-1", "Ada", []string{"admin"}, now)
	if err != nil {
		t.Fatalf("Issue expired: %v", err)
	}

	wrongAud := *signer
	wrongAud.Audience = "otomo:player"
	wrongAudience, err := wrongAud.Issue("staff-1", "Ada", []string{"admin"}, now)
	if err != nil {
		t.Fatalf("Issue wrong aud: %v", err)
	}

	wrongIss := *signer
	wrongIss.Issuer = "https://other.example"
	wrongIssuer, err := wrongIss.Issue("staff-1", "Ada", []string{"admin"}, now)
	if err != nil {
		t.Fatalf("Issue wrong iss: %v", err)
	}

	unknownKid := *signer
	unknownKid.Kid = "not-in-the-jwks"
	unknownKidToken, err := unknownKid.Issue("staff-1", "Ada", []string{"admin"}, now)
	if err != nil {
		t.Fatalf("Issue unknown kid: %v", err)
	}

	noExp := jwt.NewWithClaims(jwt.SigningMethodEdDSA, token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:   testIssuer,
			Audience: jwt.ClaimStrings{testAudience},
			Subject:  "staff-1",
			IssuedAt: jwt.NewNumericDate(now),
		},
	})
	noExp.Header["kid"] = testKid
	noExpToken, err := noExp.SignedString(signer.PrivateKey)
	if err != nil {
		t.Fatalf("sign no-exp token: %v", err)
	}

	cases := []struct {
		name   string
		token  string
		reason string
	}{
		{"missing", "", token.ReasonMissingToken},
		{"tampered signature", tampered, token.ReasonInvalidSignature},
		{"expired", expired, token.ReasonExpired},
		{"wrong audience", wrongAudience, token.ReasonAudienceMismatch},
		{"wrong issuer", wrongIssuer, token.ReasonIssuerMismatch},
		{"unknown kid", unknownKidToken, token.ReasonInvalidSignature},
		{"missing exp", noExpToken, token.ReasonInvalidToken},
		{"garbage", "not.a.jwt", token.ReasonInvalidToken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, reason := v.Verify(c.token)
			if reason != c.reason {
				t.Errorf("reason = %q, want %q", reason, c.reason)
			}
		})
	}

	// The key set is what resolves kids: after a swap the old key no longer verifies.
	v.SwapKeys([]token.PublicKey{{Kid: "some-other", Key: keys[0].Key}})
	if _, reason := v.Verify(valid); reason != token.ReasonInvalidSignature {
		t.Errorf("reason after a swap = %q, want invalid_signature", reason)
	}
}

func TestReasonForMapsUnknownErrorsToInvalidToken(t *testing.T) {
	if got := token.ReasonFor(errors.New("boom")); got != token.ReasonInvalidToken {
		t.Errorf("ReasonFor(non-jwt error) = %q, want invalid_token", got)
	}
}

// tamperSignature flips one significant character in the JWT's signature segment.
// The last character of a base64url signature may carry only padding bits, so
// tampering there can decode to the same signature; the middle of the segment cannot.
func tamperSignature(t *testing.T, signed string) string {
	t.Helper()

	dot := strings.LastIndex(signed, ".")
	if dot < 0 || dot+10 >= len(signed) {
		t.Fatalf("token %q has no signature segment", signed)
	}
	i := dot + 10
	replacement := byte('A')
	if signed[i] == 'A' {
		replacement = 'B'
	}
	return signed[:i] + string(replacement) + signed[i+1:]
}
