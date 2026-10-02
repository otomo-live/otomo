// The public half of the signing key, in the form every verifier fetches it.
//
// Copied from services/allocator/internal/token/jwks.go, minus the serving half: the
// proxy is a verifier, so it needs to parse a JWK Set and build the key map, not to
// publish one. JWKS remains so a test can serve a document in exactly the Allocator's
// shape.

package ticket

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// JWK is one key in JSON Web Key form. The fixed values are those RFC 8037 assigns to
// Ed25519: kty "OKP", crv "Ed25519", alg "EdDSA". Field order here is the field order
// of the marshalled document, which is what makes the output byte-stable.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	X   string `json:"x"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

// JWKSet is the document served at the JWKS endpoint.
type JWKSet struct {
	Keys []JWK `json:"keys"`
}

// JWKS marshals pub into a JWK Set whose kid is its RFC 7638 thumbprint. The same key
// always produces the same bytes, so downstream caches do not churn and the document
// can be diffed by eye.
//
// Copied from the Allocator so the proxy's tests exercise the same encoding the
// Allocator publishes.
func JWKS(pub ed25519.PublicKey) []byte {
	set := JWKSet{Keys: []JWK{{
		Kty: "OKP",
		Crv: "Ed25519",
		Kid: Thumbprint(pub),
		X:   base64.RawURLEncoding.EncodeToString(pub),
		Use: "sig",
		Alg: "EdDSA",
	}}}
	raw, err := json.Marshal(set)
	if err != nil {
		// json.Marshal cannot fail for a struct of strings. Returning nil rather than
		// panicking keeps an impossible error from taking down the process.
		return nil
	}
	return raw
}

// ParseJWKS turns a fetched JWK Set document into a map of kid to public key.
//
// Every key must be an Ed25519 OKP key with a kid and a 32-byte x; anything else is an
// error rather than a silently dropped key, because a malformed publishing endpoint is
// an operational bug that should be visible in the proxy's log, not a set that quietly
// stops accepting valid tickets. The x value is accepted with or without base64url
// padding, since the Allocator emits unpadded but another tool might not.
func ParseJWKS(raw []byte) (map[string]ed25519.PublicKey, error) {
	var set JWKSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("decode JWK set: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, errors.New("JWK set contains no keys")
	}

	keys := make(map[string]ed25519.PublicKey, len(set.Keys))
	for i, jwk := range set.Keys {
		if jwk.Kty != "OKP" || jwk.Crv != "Ed25519" {
			return nil, fmt.Errorf("JWK %d has kty %q crv %q, want OKP/Ed25519", i, jwk.Kty, jwk.Crv)
		}
		if jwk.Kid == "" {
			return nil, fmt.Errorf("JWK %d has no kid", i)
		}
		x, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(jwk.X, "="))
		if err != nil {
			return nil, fmt.Errorf("JWK %d (%s) x is not base64url", i, jwk.Kid)
		}
		if len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("JWK %d (%s) x decodes to %d bytes, want %d", i, jwk.Kid, len(x), ed25519.PublicKeySize)
		}
		keys[jwk.Kid] = ed25519.PublicKey(x)
	}
	return keys, nil
}
