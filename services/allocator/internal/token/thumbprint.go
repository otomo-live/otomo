// Key IDs. A join ticket's kid and the JWKS entry that matches it are both the RFC 7638
// thumbprint of the signing key, so a verifier can look the key up directly instead of
// trying every published key — and a rotation needs no out-of-band agreement on names.

package token

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
)

// Thumbprint returns the RFC 7638 JWK thumbprint of pub: the base64url (unpadded)
// SHA-256 of the required-members JSON `{"crv":"Ed25519","kty":"OKP","x":"..."}`.
//
// RFC 7638 §3.2 requires those members in lexicographic order with no whitespace, so
// the string is assembled here rather than by marshalling a struct — struct field
// order and HTML escaping would both be able to change the bytes, and a thumbprint
// that changes is a key ID that no longer matches a published JWKS.
func Thumbprint(pub ed25519.PublicKey) string {
	x := base64.RawURLEncoding.EncodeToString(pub)
	canonical := `{"crv":"Ed25519","kty":"OKP","x":"` + x + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
