// The reference join-ticket verifier, shared by our tests and the proxy and game-server
// teams so all three enforce the same contract.

package token

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Sentinel errors returned by Verify. Callers branch on these with errors.Is; the
// wrapped detail says what specifically was wrong.
var (
	// ErrInvalid covers a malformed token, a bad signature, a wrong issuer or
	// audience, an unexpected algorithm, or a missing required claim.
	ErrInvalid = errors.New("invalid join ticket")

	// ErrExpired means the ticket is otherwise well formed and correctly signed but
	// past exp (plus leeway). It is called out separately because it is the one
	// failure a client can act on.
	ErrExpired = errors.New("join ticket expired")

	// ErrUnknownKey means the header kid is absent or absent from the verifier's key
	// set, so the token could not be checked at all.
	ErrUnknownKey = errors.New("unknown join ticket key")
)

// Verifier checks join tickets against a set of public keys, keyed by kid.
type Verifier struct {
	keys     map[string]ed25519.PublicKey
	issuer   string
	audience string
	leeway   time.Duration
	now      func() time.Time
}

// NewVerifier returns a Verifier. keys is indexed by the kid that appears in a ticket
// header; a ticket whose kid is not in the map fails with ErrUnknownKey. now is
// injectable so tests can pin the clock; a nil now uses time.Now.
func NewVerifier(keys map[string]ed25519.PublicKey, issuer, audience string, leeway time.Duration, now func() time.Time) *Verifier {
	if now == nil {
		now = time.Now
	}
	return &Verifier{
		keys:     keys,
		issuer:   issuer,
		audience: audience,
		leeway:   leeway,
		now:      now,
	}
}

// Verify parses and validates ticket, returning its claims on success.
//
// Only EdDSA is accepted, so a token that claims "none" or a symmetric algorithm is
// rejected before any key is used. The kid must name a known key, and iss, aud and exp
// are checked with leeway; sub, alloc, srv and jti must all be non-empty.
//
// Single-use tracking is NOT here. The proxy owns the jti replay cache because it is
// the component that sees every presentation of a ticket; keeping that state out of the
// verifier lets the game server use the same code without sharing a store.
func (v *Verifier) Verify(ticket string) (*Claims, error) {
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(ticket, claims, v.keyfunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithLeeway(v.leeway),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return nil, classify(err)
	}
	if !parsed.Valid {
		return nil, fmt.Errorf("%w: token did not validate", ErrInvalid)
	}
	if claims.Subject == "" || claims.Alloc == "" || claims.Srv == "" || claims.ID == "" {
		return nil, fmt.Errorf("%w: sub, alloc, srv and jti must all be non-empty", ErrInvalid)
	}
	return claims, nil
}

// keyfunc is the jwt library's key lookup. Returning ErrUnknownKey-wrapped errors here
// is what surfaces an unknown kid as ErrUnknownKey rather than as an opaque signature
// failure.
func (v *Verifier) keyfunc(t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)
	if kid == "" {
		return nil, fmt.Errorf("token header has no kid: %w", ErrUnknownKey)
	}
	pub, ok := v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("kid %q: %w", kid, ErrUnknownKey)
	}
	return pub, nil
}

// classify maps the library's error tree onto the three sentinels. Unknown key and
// expiry are checked first because they are the failures callers act on; everything
// else — signature, algorithm, issuer, audience, malformed, missing claim — is
// ErrInvalid.
func classify(err error) error {
	switch {
	case errors.Is(err, ErrUnknownKey):
		return err
	case errors.Is(err, jwt.ErrTokenExpired):
		return fmt.Errorf("%w: %v", ErrExpired, err)
	default:
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
}
