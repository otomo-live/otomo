// In-process verification of this service's own access tokens, used by
// GET /admin-auth/me. Unlike Gateway and Config, which fetch the JWKS over the
// network, admin_auth already holds the active public keys: main loads them for the
// JWKS it publishes, so verification can reuse exactly that set with no fetch, no
// HTTP dependency, and no window where the endpoint it verifies against is down.

package token

import (
	"crypto/ed25519"
	"errors"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Rejection reasons. The set and the strings are deliberately identical to
// Gateway's (services/gateway/internal/authn and services/config/internal/auth), so
// the same token refused by any of the three services produces the same COM-5 code.
const (
	ReasonMissingToken     = "missing_token"
	ReasonInvalidSignature = "invalid_signature"
	ReasonExpired          = "expired"
	ReasonAudienceMismatch = "aud_mismatch"
	ReasonIssuerMismatch   = "iss_mismatch"
	// ReasonInvalidToken is the fallback for a token that parses but fails for a
	// reason outside that closed list: malformed structure, a missing subject, a
	// not-yet-valid nbf, or a missing exp.
	ReasonInvalidToken = "invalid_token"
)

// verifierLeeway is how far in the past exp may be and still be accepted. It
// matches the clock-skew allowance Gateway and Config apply, so a token that one
// service accepts is not refused by this one just because its host clock is behind.
const verifierLeeway = 30 * time.Second

// Verifier checks access tokens against one fixed key set. The key map is behind an
// atomic pointer so a rotation can swap it without locking the request path, and a
// request that already loaded the old map keeps using it until it returns.
type Verifier struct {
	issuer   string
	audience string
	keys     atomic.Pointer[map[string]ed25519.PublicKey]
}

// NewVerifier returns a Verifier for tokens signed by keys with one of the given
// kids. The set is normally publicKeys(active) — the same rows main builds the JWKS
// from — so verification and publication can never disagree about which kids exist.
func NewVerifier(issuer, audience string, keys []PublicKey) *Verifier {
	v := &Verifier{issuer: issuer, audience: audience}
	v.SwapKeys(keys)
	return v
}

// SwapKeys atomically replaces the key set. A future key rotation calls this once
// add/retire queries land; today it exists so the constructor and the test setup
// share one implementation.
func (v *Verifier) SwapKeys(keys []PublicKey) {
	m := make(map[string]ed25519.PublicKey, len(keys))
	for _, k := range keys {
		m[k.Kid] = k.Key
	}
	v.keys.Store(&m)
}

// Verify checks one bearer token and returns either the verified claims or the
// reason it was rejected. A "" reason means the token was accepted.
//
// The algorithm is pinned to EdDSA and the header's kid must name a key in the set,
// so an attacker cannot substitute alg=none or point at a key this service does not
// publish. iss, aud and exp are all required, with exp validated against
// verifierLeeway.
func (v *Verifier) Verify(token string) (*Claims, string) {
	if token == "" {
		return nil, ReasonMissingToken
	}

	keys := v.keys.Load()
	if keys == nil {
		return nil, ReasonInvalidToken
	}

	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims,
		func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			key, ok := (*keys)[kid]
			if !ok {
				// An unrecognised kid is an unverifiable token; ReasonFor maps this
				// to invalid_signature, which is the honest label — nothing was
				// verified because no key resolved.
				return nil, jwt.ErrTokenUnverifiable
			}
			return key, nil
		},
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithLeeway(verifierLeeway),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !parsed.Valid {
		return nil, ReasonFor(err)
	}

	// sub is the handle the handler loads the user by; a token without one could not
	// be attributed to anyone, so it is refused here.
	if claims.Subject == "" {
		return nil, ReasonInvalidToken
	}

	return claims, ""
}

// ReasonFor maps a parse failure onto one of the reasons above. The order mirrors
// Gateway's: signature and key resolution first, because a forged token's other
// claims are meaningless; issuer before audience, because the issuer is what names
// which identity domain the token actually came from.
func ReasonFor(err error) string {
	switch {
	case err == nil:
		return ReasonInvalidToken
	case errors.Is(err, jwt.ErrTokenSignatureInvalid), errors.Is(err, jwt.ErrTokenUnverifiable):
		return ReasonInvalidSignature
	case errors.Is(err, jwt.ErrTokenExpired):
		return ReasonExpired
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return ReasonIssuerMismatch
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return ReasonAudienceMismatch
	default:
		return ReasonInvalidToken
	}
}

// MessageFor is the human-readable half of the COM-5 body. It stays deliberately
// vague about which key or claim disagreed; the reason code carries what an operator
// needs, and a client does not need help forging a better token.
func MessageFor(reason string) string {
	switch reason {
	case ReasonMissingToken:
		return "an access token is required"
	case ReasonExpired:
		return "the access token has expired"
	case ReasonAudienceMismatch, ReasonIssuerMismatch:
		return "the access token was not issued for this service"
	default:
		return "the access token is not valid"
	}
}
