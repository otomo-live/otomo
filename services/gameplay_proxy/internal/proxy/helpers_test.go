// Shared helpers for the proxy's tests. Everything here is in package proxy so the
// tests can exercise the unexported handshake reasons and replay decisions directly.

package proxy

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/ticket"
)

const (
	testIssuer   = "https://allocator.otomo.internal"
	testAudience = "otomo:gameserver"
	testPlayer   = "3f1a2b4c-5d6e-4f70-8192-a3b4c5d6e7f8"
	testAlloc    = "9c8b7a65-4321-4f0e-9d8c-7b6a5c4d3e2f"
)

// mustKey generates a fresh Ed25519 pair for a test.
func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

// randomJTI returns a fresh 128-bit jti, the same shape the Allocator mints.
func randomJTI(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// signTicket builds and signs a join ticket with full control over every claim and the
// signing method, so one helper covers the valid case and every rejection a test needs.
func signTicket(t *testing.T, priv ed25519.PrivateKey, kid, issuer, audience, sub, alloc, srv, jti string, exp time.Time, method jwt.SigningMethod) string {
	t.Helper()

	claims := ticket.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{audience},
			Subject:   sub,
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ID:        jti,
		},
		Alloc: alloc,
		Srv:   srv,
	}
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = kid

	var key any = priv
	switch method {
	case jwt.SigningMethodHS256:
		key = []byte("test-secret")
	case jwt.SigningMethodNone:
		key = jwt.UnsafeAllowNoneSignatureType
	}
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign ticket: %v", err)
	}
	return signed
}

// issue signs a valid EdDSA ticket for srv with a fresh jti, the way the Allocator
// would.
func issue(t *testing.T, priv ed25519.PrivateKey, kid, srv string, exp time.Time) string {
	t.Helper()
	return signTicket(t, priv, kid, testIssuer, testAudience, testPlayer, testAlloc, srv, randomJTI(t), exp, jwt.SigningMethodEdDSA)
}

// discardLogger keeps test output free of the proxy's structured logs.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
