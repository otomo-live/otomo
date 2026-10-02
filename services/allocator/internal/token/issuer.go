// Package token owns the allocator's join-ticket key material and the tickets signed
// with it: generating and loading the Ed25519 private key, publishing its public half
// as a JWKS, issuing join tickets, and the reference verifier the Gameplay Proxy and
// game servers can use.
//
// A join ticket is short-lived and single-use. Single-use is not enforced here — the
// proxy owns the jti replay cache — so issuance and verification stay pure functions of
// their inputs and can be tested without a store.
package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Issuer signs join tickets with one key. Kid goes into the JWT header, which is what
// lets a verifier pick the right key out of the JWKS directly instead of trying each
// one — and what makes a rotation possible without a flag day.
type Issuer struct {
	priv     ed25519.PrivateKey
	issuer   string
	audience string
	ttl      time.Duration
	now      func() time.Time
	kid      string
}

// NewIssuer returns an Issuer for priv. now is injectable so a test can pin the clock
// and make iat and exp exact; a nil now uses time.Now.
func NewIssuer(priv ed25519.PrivateKey, issuer, audience string, ttl time.Duration, now func() time.Time) *Issuer {
	if now == nil {
		now = time.Now
	}
	return &Issuer{
		priv:     priv,
		issuer:   issuer,
		audience: audience,
		ttl:      ttl,
		now:      now,
		kid:      Thumbprint(Public(priv)),
	}
}

// Kid returns the RFC 7638 thumbprint of the issuer's public key, the value that goes
// in every ticket header and in the published JWKS.
func (i *Issuer) Kid() string { return i.kid }

// Issue signs a join ticket for one player and one allocation on one game server.
//
// The returned expiresAt is the exp actually written into the ticket, truncated to
// whole seconds like every JWT NumericDate, so a caller can report the same instant the
// verifier will enforce.
func (i *Issuer) Issue(playerID, allocationID, serverID string) (string, time.Time, error) {
	now := i.now()
	issuedAt := now.Truncate(time.Second)
	expiresAt := now.Add(i.ttl).Truncate(time.Second)

	jti, err := newJTI()
	if err != nil {
		return "", time.Time{}, err
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			Audience:  jwt.ClaimStrings{i.audience},
			Subject:   playerID,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			ID:        jti,
		},
		Alloc: allocationID,
		Srv:   serverID,
	})
	tok.Header["kid"] = i.kid

	signed, err := tok.SignedString(i.priv)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign join ticket: %w", err)
	}
	return signed, expiresAt, nil
}

// newJTI returns a fresh 128-bit random ID, base64url without padding. A ticket is
// single-use, and the proxy keys its replay cache on this value, so it must be
// unpredictable and must not repeat across two issues of an otherwise identical token.
func newJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate jti: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
