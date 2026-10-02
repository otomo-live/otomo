// The jti replay guard.
//
// A join ticket is single-use. The proxy is the component that sees every presentation
// of a ticket, so it remembers each accepted jti until the ticket's exp plus leeway, the
// instant the signature itself stops validating (doc 14 §5). The same client address
// retrying the handshake is not a replay: UDP drops handshakes, so the client resends
// up to five times, 500 ms apart, and each resend must get OTOK without opening a
// second session. A different address presenting the same jti has copied a ticket and
// gets OTNO 3.

package proxy

import (
	"net/netip"
	"sync"
	"time"
)

// replayDecision is what the guard tells the proxy about one presented jti.
type replayDecision int

const (
	// replayNew means the jti has not been seen inside its lifetime: accept and
	// remember it.
	replayNew replayDecision = iota

	// replaySameClient means this exact jti was already accepted from the same
	// address. It is a retry, so answer OTOK again and reuse the session.
	replaySameClient

	// replayReused means the jti was already accepted from a different address. This
	// is the copy case and is refused.
	replayReused
)

// replayEntry is one remembered jti. Storing the client address is what lets a resend
// be told apart from a copy.
type replayEntry struct {
	addr    netip.AddrPort
	expires time.Time
}

// replayGuard is the in-memory jti set. It is deliberately unbounded in count but
// bounded in time: entries are purged as they expire on every check, and a ticket lives
// only exp+leeway, so the set tracks exactly the tickets that could still be presented.
type replayGuard struct {
	mu      sync.Mutex
	entries map[string]replayEntry
}

// newReplayGuard returns an empty guard.
func newReplayGuard() *replayGuard {
	return &replayGuard{entries: make(map[string]replayEntry)}
}

// check classifies jti for addr at now, purging anything already expired. Purging here
// rather than on a timer means expiry forgets a jti exactly when the ticket's own
// signature would have stopped verifying it.
func (g *replayGuard) check(jti string, addr netip.AddrPort, now time.Time) replayDecision {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.purgeLocked(now)

	entry, seen := g.entries[jti]
	switch {
	case !seen:
		return replayNew
	case entry.addr == addr:
		return replaySameClient
	default:
		return replayReused
	}
}

// remember records jti as accepted from addr until expires.
func (g *replayGuard) remember(jti string, addr netip.AddrPort, expires time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entries[jti] = replayEntry{addr: addr, expires: expires}
}

// purgeLocked drops every entry whose lifetime has ended at now. An entry expires at
// exp+leeway, which is the last instant the verifier would still accept the ticket, so
// there is no window where a forgotten jti can be replayed.
func (g *replayGuard) purgeLocked(now time.Time) {
	for jti, entry := range g.entries {
		if !now.Before(entry.expires) {
			delete(g.entries, jti)
		}
	}
}

// len returns the number of remembered jtis. It exists for tests, which need to see
// that expiry actually forgot an entry rather than merely refusing to match it.
func (g *replayGuard) len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.entries)
}
