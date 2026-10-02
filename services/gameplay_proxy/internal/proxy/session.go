// A client session: one client address bound to one game server.
//
// Each session owns its own upstream UDP socket, connected to the server's
// internal_addr, so the kernel routes replies from that server back to the right
// client. The client address (IP and port) is the session key because that is what the
// game protocol uses after the handshake: the client rebinds the same local port for
// ENet, and the proxy forwards everything from that address to the server.

package proxy

import (
	"net"
	"net/netip"
	"sync/atomic"
	"time"
)

// session is one live client-to-server mapping. It is created by the proxy while it
// holds the sessions lock; lastActivity is atomic because the upstream reader goroutine
// and the public reader both touch it.
type session struct {
	addr     netip.AddrPort
	upstream net.Conn
	ticket   string
	last     atomic.Int64
}

// touch records activity at now, which is the clock the idle reaper compares against.
func (s *session) touch(now time.Time) {
	s.last.Store(now.UnixNano())
}

// lastActivity returns the instant of the most recent datagram in either direction. It
// is initialised at creation, so there is no zero-time window before the first packet.
func (s *session) lastActivity() time.Time {
	return time.Unix(0, s.last.Load())
}
