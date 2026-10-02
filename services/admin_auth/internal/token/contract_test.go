package token_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// consumerClaims is the shape gateway_dev and the config service decode a staff
// token's payload into. It is duplicated here on purpose: if this service's Claims
// struct drifts from what the consumers expect, this test stops compiling or stops
// asserting the field names, which is the point of a contract test.
type consumerClaims struct {
	jwt.RegisteredClaims
	Roles []string `json:"roles,omitempty"`
	Name  string   `json:"name,omitempty"`
}

func TestIssuedTokenMatchesTheConsumerContract(t *testing.T) {
	_, priv := mustKey(t)

	signer := &token.Signer{
		Kid:        testKid,
		PrivateKey: priv,
		Issuer:     testIssuer,
		Audience:   testAudience,
		TTL:        15 * time.Minute,
	}
	signed, err := signer.Issue("staff-9", "Grace Hopper", []string{"live_ops"}, time.Now())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(signed, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	var got consumerClaims
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload does not decode into the consumer shape: %v (%s)", err, payload)
	}

	if got.Issuer != "https://admin-auth.otomo.internal" {
		t.Errorf("iss = %q, want https://admin-auth.otomo.internal", got.Issuer)
	}
	if len(got.Audience) != 1 || got.Audience[0] != "otomo:staff" {
		t.Errorf("aud = %v, want a JSON array containing otomo:staff", got.Audience)
	}
	if got.Subject != "staff-9" {
		t.Errorf("sub = %q, want staff-9", got.Subject)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "live_ops" {
		t.Errorf("roles = %v, want [live_ops]", got.Roles)
	}
	if got.Name != "Grace Hopper" {
		t.Errorf("name = %q, want Grace Hopper", got.Name)
	}
	if got.ExpiresAt == nil || got.IssuedAt == nil {
		t.Error("exp and iat must be present")
	}
}
