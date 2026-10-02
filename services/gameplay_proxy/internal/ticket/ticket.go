// Package ticket holds the proxy's copy of the join-ticket contract: the claims, the
// reference Ed25519 verifier, the JWK Set parser and the live key set the proxy
// refreshes.
//
// The reference implementation lives in services/allocator/internal/token. An internal
// package of another module cannot be imported, so the files here are copied verbatim
// (and say so at the top of each one) and must be kept in step when the ticket format
// changes. The one addition is KeySet and FetchJWKS, which give the long-running proxy
// a key set it can replace without a restart; the game server keeps the fixed-map
// Verifier it already had.
package ticket
