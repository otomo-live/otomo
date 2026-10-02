// Package token owns the service's Ed25519 key material and the access tokens signed
// with it: generating and loading the private key, publishing the public halves as a
// JWKS, and issuing tokens.
//
// The private key never leaves the process. The JWKS is marshalled once into an
// atomically swapped pointer, so the endpoint that publishes it — the thing every
// other service hits on the verification path — takes no lock and touches no
// database.
package token

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"sort"
	"sync/atomic"
)

// PublicKey is one key to publish: the key ID and the Ed25519 public half.
type PublicKey struct {
	Kid string
	Key ed25519.PublicKey
}

// JWK is one key in JSON Web Key form. The fixed values are those RFC 8037 assigns
// to Ed25519: kty "OKP", crv "Ed25519", alg "EdDSA".
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	X   string `json:"x"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

// JWKSet is the document served at /.well-known/jwks.json.
type JWKSet struct {
	Keys []JWK `json:"keys"`
}

// BuildJWKS marshals keys into a JWK Set, sorted by kid so the document is
// byte-stable: the same set of keys always produces the same bytes, which keeps
// downstream caches from churning and makes the served document diffable by eye.
//
// An empty key list is not an error — it produces a valid set with no keys, and it is
// the caller's job to refuse to start in that case.
func BuildJWKS(keys []PublicKey) ([]byte, error) {
	sorted := make([]PublicKey, len(keys))
	copy(sorted, keys)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Kid < sorted[j].Kid })

	set := JWKSet{Keys: make([]JWK, 0, len(sorted))}
	for _, k := range sorted {
		set.Keys = append(set.Keys, JWK{
			Kty: "OKP",
			Crv: "Ed25519",
			Kid: k.Kid,
			X:   base64.RawURLEncoding.EncodeToString(k.Key),
			Use: "sig",
			Alg: "EdDSA",
		})
	}
	return json.Marshal(set)
}

// JWKSCache holds the marshalled JWKS for lock-free reads on the request path. The
// zero value is empty, not broken: Bytes returns nil until the first Swap, and the
// handler would serve an empty body rather than panic.
type JWKSCache struct {
	raw atomic.Pointer[[]byte]
}

// Swap replaces the cached document. Readers that already loaded the previous one
// keep it until they return, so a rotation never tears a response.
func (c *JWKSCache) Swap(raw []byte) {
	c.raw.Store(&raw)
}

// Bytes returns the cached document, or nil before the first Swap.
func (c *JWKSCache) Bytes() []byte {
	p := c.raw.Load()
	if p == nil {
		return nil
	}
	return *p
}
