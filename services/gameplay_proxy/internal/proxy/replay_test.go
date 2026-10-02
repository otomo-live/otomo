package proxy

import (
	"net/netip"
	"testing"
	"time"
)

// TestReplayGuard covers the three decisions and the expiry rule. The same address is a
// retry, a different address is a copy, and once the ticket's own lifetime has passed
// the jti is forgotten rather than merely refused.
func TestReplayGuard(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	clientA := netip.MustParseAddrPort("127.0.0.1:1000")
	clientB := netip.MustParseAddrPort("127.0.0.1:2000")
	expires := now.Add(time.Minute)

	g := newReplayGuard()

	if got := g.check("j1", clientA, now); got != replayNew {
		t.Errorf("first check = %v, want replayNew", got)
	}
	g.remember("j1", clientA, expires)

	if got := g.check("j1", clientA, now.Add(time.Second)); got != replaySameClient {
		t.Errorf("same-client resend = %v, want replaySameClient", got)
	}
	if got := g.check("j1", clientB, now.Add(time.Second)); got != replayReused {
		t.Errorf("different-client presentation = %v, want replayReused", got)
	}

	// Before the lifetime ends the entry is still there; at and after it the entry is
	// purged, so the same jti is new again.
	if got := g.check("j1", clientB, expires.Add(-time.Nanosecond)); got != replayReused {
		t.Errorf("just before expiry = %v, want replayReused", got)
	}
	if got := g.check("j1", clientB, expires); got != replayNew {
		t.Errorf("at expiry = %v, want replayNew", got)
	}
	if n := g.len(); n != 0 {
		t.Errorf("len after expiry = %d, want 0", n)
	}
}

// TestReplayGuardSeparatesJTIs makes sure remembering one ticket does not refuse
// another.
func TestReplayGuardSeparatesJTIs(t *testing.T) {
	now := time.Now()
	addr := netip.MustParseAddrPort("127.0.0.1:1000")
	g := newReplayGuard()
	g.remember("j1", addr, now.Add(time.Minute))
	if got := g.check("j2", addr, now); got != replayNew {
		t.Errorf("unseen jti = %v, want replayNew", got)
	}
}
