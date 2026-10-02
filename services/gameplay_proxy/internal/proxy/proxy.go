// The Gameplay Proxy engine.
//
// Players reach game servers only through this UDP listener. A datagram from an
// unknown address is either a handshake (accept, refuse, or drop silently) or is
// dropped; a datagram from an address with a session is forwarded to that session's
// server, except that a resend of the session's own handshake is answered OTOK again so
// a client whose answer was lost does not open a second session. The protocol is fixed
// by design/14-launch-handoff.md §8.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/directory"
	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/metrics"
	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/ticket"
)

// immediateRefresh is the shortest gap between two server-directory refreshes forced by
// a ticket naming an unknown server. It bounds how fast a stream of bad tickets can
// turn into HTTP requests (doc 14 §5 fixes the analogous JWKS gap at 10 s; the
// directory is polled every few seconds, so one second is the natural floor here).
const immediateRefresh = time.Second

// TicketLeeway is the clock skew the ticket contract allows on exp (doc 14 §5). It is
// exported because main configures the verifier with it and the proxy's replay guard
// uses it, and the two must be the same value: the guard must forget a jti at exactly
// the instant the signature stops validating, no earlier and no later.
const TicketLeeway = 5 * time.Second

// maxConcurrentHandshakes bounds the handshakes in flight. A handshake may wait on the
// Allocator (a JWKS refetch, a directory refresh), so each runs in its own goroutine and
// the read loop never blocks on HTTP; this cap keeps a flood of well-formed handshakes
// from turning into unbounded goroutines and ed25519 verifications.
const maxConcurrentHandshakes = 64

// maxDatagram is the largest UDP payload the read loop will accept. UDP over IPv4
// cannot exceed 65507 bytes of payload; reading into a buffer one jumbo datagram wide
// means a truncated packet is a protocol error, not a buffer bug.
const maxDatagram = 65535

// Config is the proxy engine's tunables. ListenAddr is the public UDP address;
// IdleTimeout is the silence after which a session is reaped; MaxSessions caps the live
// map. Now is injectable for tests; nil means time.Now.
type Config struct {
	ListenAddr  string
	IdleTimeout time.Duration
	MaxSessions int
	Now         func() time.Time
}

// Proxy is the UDP engine. Build it with New and run it with Run. It is safe for
// concurrent use once Run has started.
type Proxy struct {
	cfg  Config
	dir  *directory.Directory
	auth *Authenticator
	m    *metrics.Metrics
	log  *slog.Logger
	now  func() time.Time

	conn    *net.UDPConn
	started chan struct{}

	// mu guards sessions. It is held while a handshake decides, so two datagrams from
	// the same address cannot both create a session.
	mu       sync.Mutex
	sessions map[netip.AddrPort]*session

	replay *replayGuard

	// hsSlots is a counting semaphore for handshakes in flight (maxConcurrentHandshakes).
	hsSlots chan struct{}
}

// New returns a Proxy with an empty session table. dir and auth must already be
// constructed; they are loaded by the caller so a slow Allocator never blocks
// construction and readiness can report the load state.
func New(cfg Config, dir *directory.Directory, auth *Authenticator, m *metrics.Metrics, log *slog.Logger) *Proxy {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Proxy{
		cfg:      cfg,
		dir:      dir,
		auth:     auth,
		m:        m,
		log:      log,
		now:      cfg.Now,
		started:  make(chan struct{}),
		sessions: make(map[netip.AddrPort]*session),
		replay:   newReplayGuard(),
		hsSlots:  make(chan struct{}, maxConcurrentHandshakes),
	}
}

// Started returns a channel closed once the UDP socket is bound. Tests wait on it
// instead of polling, which is what lets them bind port 0 and read the real address.
func (p *Proxy) Started() <-chan struct{} {
	return p.started
}

// Addr returns the bound UDP address, or nil before Run has bound it. It is the
// configured address except when that asked for port 0.
func (p *Proxy) Addr() net.Addr {
	if p.conn == nil {
		return nil
	}
	return p.conn.LocalAddr()
}

// SessionCount returns the number of live sessions. It exists for the reaper's own
// tests and for a health log line.
func (p *Proxy) SessionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

// Run binds the UDP socket and serves until ctx is cancelled, then closes every
// upstream socket. It returns nil on an orderly shutdown; a bind failure is reported
// before any goroutine starts.
func (p *Proxy) Run(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", p.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("resolve PROXY_LISTEN_ADDR %q: %w", p.cfg.ListenAddr, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen on PROXY_LISTEN_ADDR %q: %w", p.cfg.ListenAddr, err)
	}
	p.conn = conn
	close(p.started)
	p.log.Info("gameplay proxy listening", slog.String("addr", conn.LocalAddr().String()))

	go p.reap(ctx)

	// Closing the public socket is what unblocks ReadFromUDPAddrPort. It runs in its
	// own goroutine because the read loop below is the one that must keep serving
	// until then.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	buf := make([]byte, maxDatagram)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				break
			}
			p.log.Warn("udp read failed", slog.Any("error", err))
			continue
		}
		// Copy out of the shared read buffer before handling: a handshake may block on
		// a directory refresh, and the next read reuses buf.
		pkt := append([]byte(nil), buf[:n]...)
		p.handle(ctx, from, pkt)
	}

	p.closeAll()
	p.log.Info("gameplay proxy stopped")
	return nil
}

// handle routes one datagram. The order matters: a datagram from an address that has a
// session is treated as game traffic first, because after a handshake the client's very
// next datagram is the game protocol and must not be parsed as a handshake.
func (p *Proxy) handle(ctx context.Context, from netip.AddrPort, pkt []byte) {
	p.mu.Lock()
	s := p.sessions[from]
	p.mu.Unlock()

	if s != nil {
		s.touch(p.now())
		// A retransmitted handshake for this session gets OTOK again and is not
		// forwarded; anything else is game traffic.
		if rawTicket, ok := DecodeHandshake(pkt); ok && rawTicket == s.ticket {
			p.reply(from, replyOK)
			return
		}
		p.forwardToServer(s, pkt)
		return
	}

	// No session: only a well-formed handshake is answered. Everything else is dropped
	// silently, so a scanner learns nothing and a stray UDP packet costs nothing.
	rawTicket, ok := DecodeHandshake(pkt)
	if !ok {
		return
	}

	// The handshake runs off the read loop: it can wait on the Allocator, and forwarding
	// for every live session must never wait on HTTP (a handshake naming an unknown kid or
	// srv would otherwise freeze all game traffic for the length of a request). When every
	// slot is taken the handshake is dropped unanswered; the client resends it (doc 14 §8).
	// Two handshakes from one address can run at once; handshake decides under p.mu, so
	// they still produce one session.
	select {
	case p.hsSlots <- struct{}{}:
		go func() {
			defer func() { <-p.hsSlots }()
			p.handshake(ctx, from, rawTicket)
		}()
	default:
		p.m.RecordHandshake(metrics.ResultBusy)
	}
}

// handshake verifies a ticket, resolves its server, admits it through the replay guard
// and either answers OTOK and opens a session or answers the matching OTNO reason. The
// session lock is taken only for the decision and the insert, never across the HTTP
// calls, so a slow Allocator cannot stall the whole proxy.
func (p *Proxy) handshake(ctx context.Context, from netip.AddrPort, rawTicket string) {
	claims, err := p.auth.Verify(ctx, rawTicket)
	if err != nil {
		reason, result := reasonInvalid, metrics.ResultInvalid
		if errors.Is(err, ticket.ErrExpired) {
			reason, result = reasonExpired, metrics.ResultExpired
		}
		p.m.RecordHandshake(result)
		p.reply(from, replyRefused(reason))
		p.log.Info("handshake rejected",
			slog.String("reason", result),
			slog.String("srv", srvOf(claims)),
			slog.String("client", from.String()))
		return
	}

	srv, ok := p.dir.Lookup(claims.Srv)
	if !ok {
		// The server may have registered since the last poll. Ask once, rate-limited by
		// the directory, and then decide on the answer.
		p.dir.RefreshIfStale(ctx, immediateRefresh)
		srv, ok = p.dir.Lookup(claims.Srv)
	}
	if !ok {
		p.m.RecordHandshake(metrics.ResultUnknownServer)
		p.reply(from, replyRefused(reasonUnknownServer))
		p.log.Info("handshake rejected",
			slog.String("reason", metrics.ResultUnknownServer),
			slog.String("srv", claims.Srv),
			slog.String("client", from.String()))
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// The limit is checked before the jti is remembered, so a refusal does not poison
	// the ticket: the client can retry the same ticket once capacity frees up.
	if len(p.sessions) >= p.cfg.MaxSessions {
		p.m.RecordHandshake(metrics.ResultFull)
		p.reply(from, replyRefused(reasonInvalid))
		p.log.Warn("handshake rejected: session limit reached",
			slog.String("srv", claims.Srv),
			slog.String("client", from.String()),
			slog.Int("max_sessions", p.cfg.MaxSessions))
		return
	}

	switch p.replay.check(claims.ID, from, p.now()) {
	case replaySameClient:
		// A retry from the same address. The session normally exists because it was
		// created on the first accept; recreate it if a reaper raced the retry so the
		// client still gets a usable mapping.
		if _, live := p.sessions[from]; !live {
			if _, err := p.openSession(from, srv.InternalAddr, rawTicket); err != nil {
				p.m.RecordHandshake(metrics.ResultInvalid)
				p.reply(from, replyRefused(reasonInvalid))
				p.log.Warn("cannot open session on handshake retry",
					slog.String("srv", claims.Srv), slog.String("client", from.String()), slog.Any("error", err))
				return
			}
		}
		p.m.RecordHandshake(metrics.ResultOK)
		p.reply(from, replyOK)
		return

	case replayReused:
		p.m.RecordHandshake(metrics.ResultReused)
		p.reply(from, replyRefused(reasonReused))
		p.log.Info("handshake rejected",
			slog.String("reason", metrics.ResultReused),
			slog.String("srv", claims.Srv),
			slog.String("client", from.String()))
		return
	}

	p.replay.remember(claims.ID, from, claims.ExpiresAt.Add(TicketLeeway))
	if _, err := p.openSession(from, srv.InternalAddr, rawTicket); err != nil {
		p.m.RecordHandshake(metrics.ResultInvalid)
		p.reply(from, replyRefused(reasonInvalid))
		p.log.Warn("cannot open session",
			slog.String("srv", claims.Srv), slog.String("client", from.String()), slog.Any("error", err))
		return
	}
	p.m.RecordHandshake(metrics.ResultOK)
	p.reply(from, replyOK)
	p.log.Info("session opened",
		slog.String("srv", srv.ServerID),
		slog.String("client", from.String()))
}

// openSession dials the game server and registers the session. The caller holds
// p.mu, which is what makes the "at most one session per address" guarantee hold.
func (p *Proxy) openSession(addr netip.AddrPort, internalAddr, rawTicket string) (*session, error) {
	upstream, err := net.Dial("udp", internalAddr)
	if err != nil {
		return nil, fmt.Errorf("dial game server %q: %w", internalAddr, err)
	}
	s := &session{addr: addr, upstream: upstream, ticket: rawTicket}
	s.touch(p.now())
	p.sessions[addr] = s
	p.m.SessionOpened()
	go p.readUpstream(s)
	return s, nil
}

// forwardToServer sends one client datagram upstream and counts it. A write error means
// the server's socket is gone; the session is left for the idle reaper to close, so a
// transient error does not drop a session that may still be usable.
func (p *Proxy) forwardToServer(s *session, pkt []byte) {
	n, err := s.upstream.Write(pkt)
	if err != nil {
		return
	}
	s.touch(p.now())
	p.m.RecordPacket(metrics.DirectionToServer, n)
}

// readUpstream pumps one session's server replies back to the client through the
// public socket, so replies always appear to come from the proxy's published port. It
// exits when the upstream socket is closed by closeSession or by shutdown.
func (p *Proxy) readUpstream(s *session) {
	buf := make([]byte, maxDatagram)
	for {
		n, err := s.upstream.Read(buf)
		if err != nil {
			return
		}
		if p.conn == nil {
			return
		}
		if _, err := p.conn.WriteToUDPAddrPort(buf[:n], s.addr); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.touch(p.now())
		p.m.RecordPacket(metrics.DirectionToClient, n)
	}
}

// reply sends a datagram to a client from the public socket. A failed answer is logged
// and dropped: the client's retry, if any, will produce another one.
func (p *Proxy) reply(to netip.AddrPort, msg []byte) {
	if _, err := p.conn.WriteToUDPAddrPort(msg, to); err != nil {
		p.log.Warn("cannot answer handshake",
			slog.String("client", to.String()), slog.Any("error", err))
	}
}

// reap closes sessions that have been silent for longer than IdleTimeout. It ticks at
// a quarter of the timeout (bounded to a sensible range) so a session is closed within
// roughly 25% of the configured window rather than up to a full period late.
func (p *Proxy) reap(ctx context.Context) {
	interval := p.cfg.IdleTimeout / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := p.now()
		p.mu.Lock()
		var expired []*session
		for _, s := range p.sessions {
			if now.Sub(s.lastActivity()) >= p.cfg.IdleTimeout {
				expired = append(expired, s)
			}
		}
		p.mu.Unlock()

		for _, s := range expired {
			p.closeSession(s)
		}
	}
}

// closeSession removes one session and closes its upstream socket, which unblocks the
// reader goroutine. It is idempotent: a session already removed by shutdown is left
// alone, so the gauge is not decremented twice.
func (p *Proxy) closeSession(s *session) {
	p.mu.Lock()
	if p.sessions[s.addr] == s {
		delete(p.sessions, s.addr)
		p.m.SessionClosed()
	}
	p.mu.Unlock()
	_ = s.upstream.Close()
}

// closeAll drops every session at shutdown and closes its socket.
func (p *Proxy) closeAll() {
	p.mu.Lock()
	sessions := make([]*session, 0, len(p.sessions))
	for _, s := range p.sessions {
		sessions = append(sessions, s)
	}
	p.sessions = make(map[netip.AddrPort]*session)
	p.mu.Unlock()

	for _, s := range sessions {
		p.m.SessionClosed()
		_ = s.upstream.Close()
	}
}

// srvOf returns the srv claim for a log line, or "" when the ticket never got far
// enough to have claims. It keeps the failure logs uniform without logging the ticket.
func srvOf(claims *ticket.Claims) string {
	if claims == nil {
		return ""
	}
	return claims.Srv
}
