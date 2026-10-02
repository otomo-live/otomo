// The public half of the signing key, in the form every verifier fetches it.

package token

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
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

// Handler serves a pre-marshalled JWKS document. Only GET and HEAD are allowed; a
// verifier fetches this on the hot path, so the response is cacheable and the body is
// written as-is rather than re-encoded per request.
func Handler(doc []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		if r.Method == http.MethodHead {
			// A HEAD response carries the headers a GET would, but no body. Setting
			// Content-Length explicitly keeps the two responses interchangeable.
			w.Header().Set("Content-Length", strconv.Itoa(len(doc)))
			return
		}
		_, _ = w.Write(doc)
	})
}
