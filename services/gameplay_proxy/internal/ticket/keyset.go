// Live key-set state and the HTTP fetch that fills it.
//
// The reference Verifier takes a fixed map, which is what the game server wants: it
// fetches once and a rotation is a restart. The proxy instead serves every player and
// cannot restart on a key rotation, so it owns a mutable KeySet and rebuilds a Verifier
// from a snapshot whenever it verifies. Snapshotting matters: a Verifier keeps its map
// for the duration of one Verify call, and a concurrent refresh must not mutate it.

package ticket

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// maxJWKSBytes bounds the response the proxy will read. A JWK Set for a handful of
// Ed25519 keys is a few hundred bytes; a megabyte is already generous, and the bound
// stops a hostile or broken peer from making the proxy allocate without limit.
const maxJWKSBytes = 1 << 20

// KeySet holds the current public keys, keyed by kid. It is safe for concurrent use:
// Replace is called by the refresher and Snapshot by every verify.
type KeySet struct {
	mu     sync.RWMutex
	keys   map[string]ed25519.PublicKey
	loaded bool
}

// NewKeySet returns an empty KeySet. Loaded reports false until the first successful
// Replace, which is what /readyz waits on.
func NewKeySet() *KeySet {
	return &KeySet{keys: make(map[string]ed25519.PublicKey)}
}

// Replace swaps the whole set for keys. A refresh is atomic from a verifier's point of
// view: it sees either the old set or the new one, never a half-applied mix.
func (k *KeySet) Replace(keys map[string]ed25519.PublicKey) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = keys
	k.loaded = true
}

// Snapshot returns a copy of the current keys. The copy is what makes it safe to keep
// the returned map in a Verifier while another goroutine replaces the set.
func (k *KeySet) Snapshot() map[string]ed25519.PublicKey {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make(map[string]ed25519.PublicKey, len(k.keys))
	for kid, pub := range k.keys {
		out[kid] = pub
	}
	return out
}

// Loaded reports whether at least one key set has been installed. It stays true across
// a failed refresh, so a transient Allocator outage does not make the proxy unready
// after it has already served successfully.
func (k *KeySet) Loaded() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.loaded
}

// FetchJWKS downloads and parses the JWK Set at url. It is the one place the proxy
// speaks the Allocator's key-distribution protocol, so a change to the endpoint shape
// is a change here and nowhere else. The body is bounded and the caller supplies the
// context deadline.
func FetchJWKS(ctx context.Context, client *http.Client, url string) (map[string]ed25519.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build JWKS request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch JWKS: unexpected status %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return nil, fmt.Errorf("read JWKS: %w", err)
	}
	return ParseJWKS(raw)
}
