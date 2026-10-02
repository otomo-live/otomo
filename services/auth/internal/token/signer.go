// Access-token issuance.

package token

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalidSubject is returned by Issue for a subject that is not a canonical,
// non-nil UUID. A caller that passes one has a bug: the subject is always an
// account_id read back from Postgres.
var ErrInvalidSubject = errors.New("subject must be a canonical non-nil UUID")

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

// Issue signs an access token for subject sub, valid from now until now+TTL.
//
// now is a parameter rather than a call to time.Now inside, so a test can pin it and
// assert on exact timestamps, and so exp, nbf and iat are all derived from one instant
// and cannot disagree.
//
// sub must be an account_id in canonical form (lowercase, hyphenated). Session keys
// player_profile on it and refuses any token whose sub does not parse as a UUID, so a
// token with an empty or non-UUID subject would pass Gateway and then 401 at every
// Session route. Requiring the canonical spelling as well means one account can never
// appear under two different sub strings.
func (s *Signer) Issue(sub string, now time.Time) (string, error) {
	if id, err := uuid.Parse(sub); err != nil || id == uuid.Nil() || id.String() != sub {
		return "", fmt.Errorf("sign access token for %q: %w", sub, ErrInvalidSubject)
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
	})
	tok.Header["kid"] = s.Kid

	signed, err := tok.SignedString(s.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("sign access token: %w", err)
	}
	return signed, nil
}
