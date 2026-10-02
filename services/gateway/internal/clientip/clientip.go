// Package clientip resolves the real client address for requests that arrive
// through a trusted reverse proxy, so the rate limiter, the access log and the
// forwarding proxy all key on the player rather than on the proxy.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Middleware rewrites r.RemoteAddr to the client named by X-Forwarded-For, but
// only when the request's TCP peer is inside one of the trusted prefixes. It
// belongs outermost, before request-ID, logging and recover, so every inner
// layer sees the resolved address.
//
// The header is read only from a trusted peer: an arbitrary client can send any
// X-Forwarded-For it likes, and believing it would let one client assume other
// players' identities and dodge the per-IP limit. An untrusted request is
// passed through untouched, header included, exactly as before this middleware
// existed.
//
// When the peer is trusted, the rightmost non-empty entry is used, not the
// leftmost. Each proxy appends the address it received the request from, so the
// rightmost entry was written by the nearest trusted hop; entries to its left
// may still have been supplied by the client. The edge in front of this gateway
// overwrites the header rather than appending, so in practice there is one
// entry, but taking the rightmost stays correct if another trusted hop is ever
// added.
//
// The header is deleted before the request continues. internal/proxy derives
// its outbound X-Forwarded-For from RemoteAddr, so leaving the inbound value in
// place would let a downstream service re-trust a chain this layer has already
// replaced.
func Middleware(trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, ok := peerAddr(r.RemoteAddr)
		if !ok || !trustedPeer(trusted, peer) {
			next.ServeHTTP(w, r)
			return
		}
		// A trusted peer with no header is not claiming a client.
		if r.Header.Get("X-Forwarded-For") == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Several X-Forwarded-For lines are one list in order (RFC 9110 §5.3), so
		// the rightmost entry is at the end of the last line.
		ip, ok := rightmostAddr(strings.Join(r.Header.Values("X-Forwarded-For"), ","))
		clone := r.Clone(r.Context())
		clone.Header.Del("X-Forwarded-For")
		if ok {
			// The port is irrelevant and cannot be recovered from the header;
			// "0" keeps SplitHostPort working for the rate limiter and the log.
			clone.RemoteAddr = net.JoinHostPort(ip.String(), "0")
		}
		next.ServeHTTP(w, clone)
	})
}

// peerAddr extracts the IP from a RemoteAddr, tolerating a missing port. It
// unmaps IPv4-in-IPv6 so a dual-stack listener's peer matches an IPv4 prefix.
func peerAddr(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// trustedPeer reports whether peer is inside any trusted prefix.
func trustedPeer(trusted []netip.Prefix, peer netip.Addr) bool {
	for _, p := range trusted {
		if p.Contains(peer) {
			return true
		}
	}
	return false
}

// rightmostAddr returns the last non-empty comma-separated entry, parsed and
// IPv4-unmapped. It reports false for an all-empty header or when the rightmost
// entry is not an IP, in which case the caller keeps the peer address.
func rightmostAddr(xff string) (netip.Addr, bool) {
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		s := strings.TrimSpace(parts[i])
		if s == "" {
			continue
		}
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Addr{}, false
		}
		return ip.Unmap(), true
	}
	return netip.Addr{}, false
}
