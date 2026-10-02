// GET /.well-known/jwks.json — the public half of the signing keys.
//
// It has to be reachable without a token, because a verifier needs the keys before it
// has any credential to present — which is also why the document it serves is public
// information by design, and not a leak.

package api

import "net/http"

// JWKSProvider is the read side of the JWKS cache: the document as bytes, already
// marshalled. *token.JWKSCache satisfies it.
type JWKSProvider interface {
	Bytes() []byte
}

// JWKS serves the provider's cached document verbatim — no marshalling per request,
// and no database access on the verification path.
//
// Cache-Control is no-store on purpose. The document is tiny and fetched rarely, and
// a copy cached past a key rotation is a verification failure that looks like a
// signing bug rather than a stale cache.
func JWKS(provider JWKSProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(provider.Bytes())
	}
}
