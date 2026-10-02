// Access-token issuance.

package token

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Signer issues access tokens with one key. The Kid goes into the JWT header, which
// is what lets a verifier pick the right key out of the JWKS directly instead of
// trying each one — and what makes a rotation possible without a flag day.
type Signer struct {
	Kid        string
	PrivateKey ed25519.PrivateKey
	Issuer     string
	Audience   string
	TTL        time.Duration
}

// Issue signs a staff access token for subject sub, carrying name and roles, valid
// from now until now+TTL.
//
// now is a parameter rather than a call to time.Now inside, so a test can pin it and
// assert on exact timestamps, and so exp, nbf and iat are all derived from one instant
// and cannot disagree.
//
// An empty roles slice is rejected rather than signed: every staff token carries the
// roles its bearer acts with, and a token with none would be a claim-set bug that
// downstream authorization reads as "no permissions" instead of failing here.
func (s *Signer) Issue(sub, name string, roles []string, now time.Time) (string, error) {
	if len(roles) == 0 {
		return "", errors.New("issue access token: roles must not be empty")
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.Issuer,
			Audience:  jwt.ClaimStrings{s.Audience},
			Subject:   sub,
			ExpiresAt: jwt.NewNumericDate(now.Add(s.TTL)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
		},
		Roles: roles,
		Name:  name,
	})
	tok.Header["kid"] = s.Kid

	signed, err := tok.SignedString(s.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("sign access token: %w", err)
	}
	return signed, nil
}
