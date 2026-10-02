package clientip

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"gateway/gateway/internal/ratelimit"
)

var ok = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

func request(h http.Handler, remoteAddr, xff string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	h.ServeHTTP(rr, req)
	return rr
}

// run evaluates the middleware with a capture handler and reports what the
// inner handler saw. hasXFF uses Values, because Get cannot tell a deleted
// header from an empty one.
func run(t *testing.T, trusted []netip.Prefix, remoteAddr, xff string) (remote string, hasXFF bool, xffVal string) {
	t.Helper()
	h := Middleware(trusted, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote = r.RemoteAddr
		hasXFF = len(r.Header.Values("X-Forwarded-For")) > 0
		xffVal = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	}))
	request(h, remoteAddr, xff)
	return remote, hasXFF, xffVal
}

// An untrusted peer's X-Forwarded-For is a spoof: it must not change the
// address and must not be touched, exactly as before this middleware existed.
func TestMiddleware_UntrustedPeerUnchanged(t *testing.T) {
	remote, hasXFF, xffVal := run(t, prefixes(t, "10.0.0.0/8"), "203.0.113.9:5555", "198.51.100.7")
	if remote != "203.0.113.9:5555" {
		t.Errorf("RemoteAddr = %q, want the peer %q", remote, "203.0.113.9:5555")
	}
	if !hasXFF || xffVal != "198.51.100.7" {
		t.Errorf("X-Forwarded-For present=%v value=%q, want the spoofed header left alone", hasXFF, xffVal)
	}
}

// A trusted peer's header is believed: RemoteAddr becomes the resolved client
// with a placeholder port, and the header is deleted so nothing downstream
// re-trusts it.
func TestMiddleware_TrustedPeerUsesRightmost(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8")
	tests := []struct {
		name string
		xff  string
		want string
	}{
		{"single entry", "198.51.100.7", "198.51.100.7:0"},
		{"multiple entries use the rightmost", "1.1.1.1, 2.2.2.2, 198.51.100.7", "198.51.100.7:0"},
		{"trailing empty entries are skipped", "1.1.1.1, 198.51.100.7 , ", "198.51.100.7:0"},
		{"IPv4-in-IPv6 is unmapped", "::ffff:198.51.100.7", "198.51.100.7:0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote, hasXFF, _ := run(t, trusted, "10.0.0.5:5555", tt.xff)
			if remote != tt.want {
				t.Errorf("RemoteAddr = %q, want %q", remote, tt.want)
			}
			if hasXFF {
				t.Error("X-Forwarded-For was not deleted")
			}
		})
	}
}

// A rightmost entry that is not an IP is not a client name: the peer stands,
// and the header is still deleted because it is untrustworthy either way.
func TestMiddleware_GarbageFallsBackToPeer(t *testing.T) {
	for _, xff := range []string{"1.1.1.1, not-an-ip", ", ,", "not-an-ip"} {
		t.Run(xff, func(t *testing.T) {
			remote, hasXFF, _ := run(t, prefixes(t, "10.0.0.0/8"), "10.0.0.5:5555", xff)
			if remote != "10.0.0.5:5555" {
				t.Errorf("RemoteAddr = %q, want the peer %q", remote, "10.0.0.5:5555")
			}
			if hasXFF {
				t.Error("X-Forwarded-For was not deleted")
			}
		})
	}
}

// An IPv6 peer is matched against an IPv6 prefix, and an IPv6 client is
// rewritten with the bracketed form SplitHostPort expects.
func TestMiddleware_IPv6PeerAndClient(t *testing.T) {
	remote, hasXFF, _ := run(t, prefixes(t, "2001:db8::/32"), "[2001:db8::5]:5555", "2001:db8:abcd::7")
	if remote != "[2001:db8:abcd::7]:0" {
		t.Errorf("RemoteAddr = %q, want %q", remote, "[2001:db8:abcd::7]:0")
	}
	if hasXFF {
		t.Error("X-Forwarded-For was not deleted")
	}
}

// The default configuration trusts nobody: even a peer that looks like a proxy
// changes nothing and keeps its header.
func TestMiddleware_EmptyTrustedListIsNoOp(t *testing.T) {
	remote, hasXFF, xffVal := run(t, nil, "10.0.0.5:5555", "198.51.100.7")
	if remote != "10.0.0.5:5555" {
		t.Errorf("RemoteAddr = %q, want the peer unchanged", remote)
	}
	if !hasXFF || xffVal != "198.51.100.7" {
		t.Errorf("X-Forwarded-For present=%v value=%q, want untouched", hasXFF, xffVal)
	}
}

// A trusted peer that sends no header keeps its own address; there is nothing
// to resolve, so the request is not cloned.
func TestMiddleware_TrustedPeerWithoutHeaderKeepsPeer(t *testing.T) {
	remote, hasXFF, _ := run(t, prefixes(t, "10.0.0.0/8"), "10.0.0.5:5555", "")
	if remote != "10.0.0.5:5555" {
		t.Errorf("RemoteAddr = %q, want the peer unchanged", remote)
	}
	if hasXFF {
		t.Error("X-Forwarded-For should not appear from nowhere")
	}
}

// Composition with the real limiter: two clients behind one trusted peer get
// separate buckets, while the same client's second immediate request is 429.
func TestMiddleware_RateLimitBucketsPerResolvedClient(t *testing.T) {
	h := Middleware(prefixes(t, "10.0.0.0/8"), ratelimit.New(0, 1).Wrap(ok))

	if rr := request(h, "10.0.0.1:1111", "203.0.113.1"); rr.Code != http.StatusOK {
		t.Fatalf("client A first request: %d, want 200", rr.Code)
	}
	if rr := request(h, "10.0.0.1:2222", "203.0.113.2"); rr.Code != http.StatusOK {
		t.Errorf("client B first request: %d, want 200 (separate bucket)", rr.Code)
	}
	if rr := request(h, "10.0.0.1:3333", "203.0.113.1"); rr.Code != http.StatusTooManyRequests {
		t.Errorf("client A second request: %d, want 429 (its own burst is spent)", rr.Code)
	}
}

// The untrusted counterpart: the peer's header is ignored, so two different
// claimed clients share the one bucket keyed on the peer address.
func TestMiddleware_UntrustedPeersShareThePeerBucket(t *testing.T) {
	h := Middleware(prefixes(t, "10.0.0.0/8"), ratelimit.New(0, 1).Wrap(ok))

	if rr := request(h, "198.51.100.9:1111", "203.0.113.1"); rr.Code != http.StatusOK {
		t.Fatalf("untrusted first request: %d, want 200", rr.Code)
	}
	if rr := request(h, "198.51.100.9:2222", "203.0.113.2"); rr.Code != http.StatusTooManyRequests {
		t.Errorf("untrusted second request with a new spoofed XFF: %d, want 429 (peer bucket)", rr.Code)
	}
}
