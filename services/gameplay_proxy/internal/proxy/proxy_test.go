package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/directory"
	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/metrics"
	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/ticket"
)

// serverEntry is the Allocator directory's JSON shape, repeated here so the fake
// Allocator encodes exactly what the real one documents.
type serverEntry struct {
	ServerID     string `json:"server_id"`
	InternalAddr string `json:"internal_addr"`
	State        string `json:"state"`
}

// fakeAllocator is an httptest server speaking the two endpoints the proxy uses. It
// enforces the bearer key so a test fails if the proxy forgets to send it.
type fakeAllocator struct {
	server *httptest.Server
	key    string

	mu           sync.Mutex
	servers      []serverEntry
	jwks         []byte
	unauthorized int
	// delay, when set, holds every answer that long: a slow Allocator.
	delay time.Duration
}

func (fa *fakeAllocator) setDelay(d time.Duration) {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	fa.delay = d
}

func (fa *fakeAllocator) wait() {
	fa.mu.Lock()
	d := fa.delay
	fa.mu.Unlock()
	time.Sleep(d)
}

func newFakeAllocator(t *testing.T, pub ed25519.PublicKey, key string) *fakeAllocator {
	t.Helper()
	fa := &fakeAllocator{key: key, jwks: ticket.JWKS(pub)}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		fa.wait()
		fa.mu.Lock()
		doc := fa.jwks
		fa.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
	mux.HandleFunc("/internal/servers", func(w http.ResponseWriter, r *http.Request) {
		fa.wait()
		if r.Header.Get("Authorization") != "Bearer "+fa.key {
			fa.mu.Lock()
			fa.unauthorized++
			fa.mu.Unlock()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fa.mu.Lock()
		entries := append([]serverEntry(nil), fa.servers...)
		fa.mu.Unlock()
		_ = json.NewEncoder(w).Encode(struct {
			Servers []serverEntry `json:"servers"`
		}{entries})
	})

	fa.server = httptest.NewServer(mux)
	t.Cleanup(fa.server.Close)
	return fa
}

func (fa *fakeAllocator) setServers(entries ...serverEntry) {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	fa.servers = entries
}

// testEnv is a running proxy wired to a fake Allocator, with the pieces a test needs to
// assert on.
type testEnv struct {
	proxy     *Proxy
	auth      *Authenticator
	dir       *directory.Directory
	metrics   *metrics.Metrics
	allocator *fakeAllocator
	addr      string
	registry  *prometheus.Registry
}

// startTestEnv loads the JWKS and directory synchronously, then starts the UDP listener
// on a random loopback port. dirNow injects the directory's clock; nil uses time.Now.
func startTestEnv(t *testing.T, fa *fakeAllocator, idle time.Duration, maxSessions int, dirNow func() time.Time) *testEnv {
	t.Helper()

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	dir := directory.New(directory.Config{
		URL:     fa.server.URL + "/internal/servers",
		Key:     fa.key,
		Refresh: time.Hour,
		Client:  fa.server.Client(),
		Now:     dirNow,
		Metrics: m,
		Logger:  discardLogger(),
	})
	if err := dir.Refresh(t.Context()); err != nil {
		t.Fatalf("initial directory refresh: %v", err)
	}

	auth := NewAuthenticator(AuthenticatorConfig{
		JWKSURL:         fa.server.URL + "/.well-known/jwks.json",
		Issuer:          testIssuer,
		Audience:        testAudience,
		Leeway:          TicketLeeway,
		RefreshInterval: 10 * time.Second,
		Client:          fa.server.Client(),
		Logger:          discardLogger(),
	})
	if err := auth.Load(t.Context()); err != nil {
		t.Fatalf("initial JWKS load: %v", err)
	}

	p := New(Config{
		ListenAddr:  "127.0.0.1:0",
		IdleTimeout: idle,
		MaxSessions: maxSessions,
	}, dir, auth, m, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = p.Run(ctx) }()
	select {
	case <-p.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not start")
	}
	t.Cleanup(cancel)

	return &testEnv{
		proxy:     p,
		auth:      auth,
		dir:       dir,
		metrics:   m,
		allocator: fa,
		addr:      p.Addr().String(),
		registry:  reg,
	}
}

// TestEndToEndHandshakeAndForward is the core path: a valid ticket is accepted, the
// client's payload reaches the echo server through the proxy, and the echo comes back.
func TestEndToEndHandshakeAndForward(t *testing.T) {
	pub, priv := mustKey(t)
	echo := startEcho(t)
	fa := newFakeAllocator(t, pub, "test-key-0123456789")
	fa.setServers(serverEntry{ServerID: "gs-1", InternalAddr: echo, State: "busy"})
	env := startTestEnv(t, fa, 30*time.Second, 100, nil)

	c := dialUDP(t, env.addr)
	tok := issue(t, priv, ticket.Thumbprint(pub), "gs-1", time.Now().Add(time.Minute))
	if got := string(handshake(t, c, tok)); got != "OTOK" {
		t.Fatalf("handshake answer = %q, want OTOK", got)
	}
	if got := string(sendAndRead(t, c, []byte("hello"))); got != "hello" {
		t.Fatalf("forwarded echo = %q, want hello", got)
	}

	if n := env.proxy.SessionCount(); n != 1 {
		t.Errorf("SessionCount = %d, want 1", n)
	}
	if got := testutil.ToFloat64(env.metrics.HandshakeCounter(metrics.ResultOK)); got != 1 {
		t.Errorf("ok handshakes = %v, want 1", got)
	}
	if got := testutil.ToFloat64(env.metrics.HandshakeCounter(metrics.ResultInvalid)); got != 0 {
		t.Errorf("invalid handshakes = %v, want 0", got)
	}
	if got := testutil.ToFloat64(env.metrics.SessionsGauge()); got != 1 {
		t.Errorf("sessions gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(env.metrics.DirectoryServersGauge()); got != 1 {
		t.Errorf("directory servers gauge = %v, want 1", got)
	}
}

// TestEndToEndSecondClientHasOwnSession checks that two addresses get two independent
// mappings to the same server.
func TestEndToEndSecondClientHasOwnSession(t *testing.T) {
	pub, priv := mustKey(t)
	echo := startEcho(t)
	fa := newFakeAllocator(t, pub, "test-key-0123456789")
	fa.setServers(serverEntry{ServerID: "gs-1", InternalAddr: echo, State: "busy"})
	env := startTestEnv(t, fa, 30*time.Second, 100, nil)

	kid := ticket.Thumbprint(pub)
	c1 := dialUDP(t, env.addr)
	c2 := dialUDP(t, env.addr)
	if got := string(handshake(t, c1, issue(t, priv, kid, "gs-1", time.Now().Add(time.Minute)))); got != "OTOK" {
		t.Fatalf("first client answer = %q, want OTOK", got)
	}
	if got := string(handshake(t, c2, issue(t, priv, kid, "gs-1", time.Now().Add(time.Minute)))); got != "OTOK" {
		t.Fatalf("second client answer = %q, want OTOK", got)
	}
	if n := env.proxy.SessionCount(); n != 2 {
		t.Errorf("SessionCount = %d, want 2", n)
	}
	if got := string(sendAndRead(t, c1, []byte("one"))); got != "one" {
		t.Errorf("first client echo = %q, want one", got)
	}
	if got := string(sendAndRead(t, c2, []byte("two"))); got != "two" {
		t.Errorf("second client echo = %q, want two", got)
	}
}

// TestEndToEndRefusals checks the OTNO reasons the proxy can answer without state:
// invalid, expired and unknown server.
func TestEndToEndRefusals(t *testing.T) {
	pub, priv := mustKey(t)
	echo := startEcho(t)
	fa := newFakeAllocator(t, pub, "test-key-0123456789")
	fa.setServers(serverEntry{ServerID: "gs-1", InternalAddr: echo, State: "busy"})
	env := startTestEnv(t, fa, 30*time.Second, 100, nil)

	kid := ticket.Thumbprint(pub)
	c := dialUDP(t, env.addr)

	cases := []struct {
		name string
		tok  string
		want string
	}{
		{"invalid", "not-a-jwt", "OTNO\x01"},
		{"expired", issue(t, priv, kid, "gs-1", time.Now().Add(-time.Minute)), "OTNO\x02"},
		{"unknown server", issue(t, priv, kid, "gs-nope", time.Now().Add(time.Minute)), "OTNO\x04"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(handshake(t, c, tc.tok)); got != tc.want {
				t.Fatalf("answer = %q, want %q", got, tc.want)
			}
		})
	}

	if n := env.proxy.SessionCount(); n != 0 {
		t.Errorf("SessionCount = %d, want 0 after refusals", n)
	}
	if got := testutil.ToFloat64(env.metrics.HandshakeCounter(metrics.ResultInvalid)); got != 1 {
		t.Errorf("invalid handshakes = %v, want 1", got)
	}
	if got := testutil.ToFloat64(env.metrics.HandshakeCounter(metrics.ResultExpired)); got != 1 {
		t.Errorf("expired handshakes = %v, want 1", got)
	}
	if got := testutil.ToFloat64(env.metrics.HandshakeCounter(metrics.ResultUnknownServer)); got != 1 {
		t.Errorf("unknown-server handshakes = %v, want 1", got)
	}
}

// TestEndToEndReplay covers single use: the same ticket from a different address is
// reused, while the same address resending its own handshake still gets OTOK and keeps
// one session.
func TestEndToEndReplay(t *testing.T) {
	pub, priv := mustKey(t)
	echo := startEcho(t)
	fa := newFakeAllocator(t, pub, "test-key-0123456789")
	fa.setServers(serverEntry{ServerID: "gs-1", InternalAddr: echo, State: "busy"})
	env := startTestEnv(t, fa, 30*time.Second, 100, nil)

	tok := issue(t, priv, ticket.Thumbprint(pub), "gs-1", time.Now().Add(time.Minute))

	clientA := dialUDP(t, env.addr)
	if got := string(handshake(t, clientA, tok)); got != "OTOK" {
		t.Fatalf("first presentation = %q, want OTOK", got)
	}
	// The same client retrying must not open a second session.
	if got := string(handshake(t, clientA, tok)); got != "OTOK" {
		t.Fatalf("same-client resend = %q, want OTOK", got)
	}
	if n := env.proxy.SessionCount(); n != 1 {
		t.Errorf("SessionCount after resend = %d, want 1", n)
	}

	clientB := dialUDP(t, env.addr)
	if got := string(handshake(t, clientB, tok)); got != "OTNO\x03" {
		t.Fatalf("different client = %q, want OTNO 3", got)
	}
	if got := testutil.ToFloat64(env.metrics.HandshakeCounter(metrics.ResultReused)); got != 1 {
		t.Errorf("reused handshakes = %v, want 1", got)
	}
}

// TestEndToEndUnknownSourceDropped checks that a datagram from an address with no
// session is ignored unless it is a well-formed handshake.
func TestEndToEndUnknownSourceDropped(t *testing.T) {
	pub, _ := mustKey(t)
	echo := startEcho(t)
	fa := newFakeAllocator(t, pub, "test-key-0123456789")
	fa.setServers(serverEntry{ServerID: "gs-1", InternalAddr: echo, State: "busy"})
	env := startTestEnv(t, fa, 30*time.Second, 100, nil)

	c := dialUDP(t, env.addr)
	for _, msg := range [][]byte{
		[]byte("not a handshake"),
		[]byte("XXXX\x00\x03abc"), // right shape, wrong magic
	} {
		if _, err := c.Write(msg); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		buf := make([]byte, 64)
		if n, err := c.Read(buf); err == nil {
			t.Fatalf("got reply %q to a non-handshake, want no reply", buf[:n])
		}
	}
	if n := env.proxy.SessionCount(); n != 0 {
		t.Errorf("SessionCount = %d, want 0", n)
	}
}

// TestEndToEndIdleTimeout checks that a silent session is reaped and that the same
// address is then an unknown source again.
func TestEndToEndIdleTimeout(t *testing.T) {
	pub, priv := mustKey(t)
	echo := startEcho(t)
	fa := newFakeAllocator(t, pub, "test-key-0123456789")
	fa.setServers(serverEntry{ServerID: "gs-1", InternalAddr: echo, State: "busy"})
	env := startTestEnv(t, fa, 200*time.Millisecond, 100, nil)

	c := dialUDP(t, env.addr)
	tok := issue(t, priv, ticket.Thumbprint(pub), "gs-1", time.Now().Add(time.Minute))
	if got := string(handshake(t, c, tok)); got != "OTOK" {
		t.Fatalf("handshake answer = %q, want OTOK", got)
	}
	if got := string(sendAndRead(t, c, []byte("hi"))); got != "hi" {
		t.Fatalf("echo = %q, want hi", got)
	}

	deadline := time.Now().Add(3 * time.Second)
	for env.proxy.SessionCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := env.proxy.SessionCount(); n != 0 {
		t.Fatalf("SessionCount = %d, want the idle session reaped", n)
	}
	if got := testutil.ToFloat64(env.metrics.SessionsGauge()); got != 0 {
		t.Errorf("sessions gauge = %v, want 0", got)
	}

	// The address now has no session, so a non-handshake datagram is dropped.
	if _, err := c.Write([]byte("still there?")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 64)
	if n, err := c.Read(buf); err == nil {
		t.Fatalf("got reply %q after reap, want none", buf[:n])
	}
}

// TestEndToEndDirectoryImmediateRefresh checks that a server added after the proxy
// started is found when a ticket names it, via the rate-limited immediate refresh
// rather than waiting for the poll.
func TestEndToEndDirectoryImmediateRefresh(t *testing.T) {
	pub, priv := mustKey(t)
	echoEarly := startEcho(t)
	echoLate := startEcho(t)
	fa := newFakeAllocator(t, pub, "test-key-0123456789")
	fa.setServers(serverEntry{ServerID: "gs-early", InternalAddr: echoEarly, State: "busy"})

	clock := newFakeClock(time.Now())
	env := startTestEnv(t, fa, 30*time.Second, 100, clock.Now)

	// The new server registers after the proxy's last poll; the clock advances past the
	// one-second immediate-refresh floor so the handshake is allowed to ask again.
	fa.setServers(
		serverEntry{ServerID: "gs-early", InternalAddr: echoEarly, State: "busy"},
		serverEntry{ServerID: "gs-late", InternalAddr: echoLate, State: "busy"},
	)
	clock.Advance(2 * time.Second)

	c := dialUDP(t, env.addr)
	tok := issue(t, priv, ticket.Thumbprint(pub), "gs-late", time.Now().Add(time.Minute))
	if got := string(handshake(t, c, tok)); got != "OTOK" {
		t.Fatalf("handshake for a newly added server = %q, want OTOK", got)
	}
	if got := string(sendAndRead(t, c, []byte("late"))); got != "late" {
		t.Fatalf("echo through the new server = %q, want late", got)
	}
	if got := testutil.ToFloat64(env.metrics.DirectoryServersGauge()); got != 2 {
		t.Errorf("directory servers gauge = %v, want 2", got)
	}
	if got := testutil.ToFloat64(env.metrics.HandshakeCounter(metrics.ResultUnknownServer)); got != 0 {
		t.Errorf("unknown-server handshakes = %v, want 0", got)
	}
}

// TestEndToEndFull checks the session cap: a new handshake beyond MaxSessions is
// refused with OTNO 1 and counted as full.
func TestEndToEndFull(t *testing.T) {
	pub, priv := mustKey(t)
	echo := startEcho(t)
	fa := newFakeAllocator(t, pub, "test-key-0123456789")
	fa.setServers(serverEntry{ServerID: "gs-1", InternalAddr: echo, State: "busy"})
	env := startTestEnv(t, fa, 30*time.Second, 1, nil)

	kid := ticket.Thumbprint(pub)
	clientA := dialUDP(t, env.addr)
	if got := string(handshake(t, clientA, issue(t, priv, kid, "gs-1", time.Now().Add(time.Minute)))); got != "OTOK" {
		t.Fatalf("first client = %q, want OTOK", got)
	}

	clientB := dialUDP(t, env.addr)
	if got := string(handshake(t, clientB, issue(t, priv, kid, "gs-1", time.Now().Add(time.Minute)))); got != "OTNO\x01" {
		t.Fatalf("second client = %q, want OTNO 1", got)
	}
	if n := env.proxy.SessionCount(); n != 1 {
		t.Errorf("SessionCount = %d, want 1", n)
	}
	if got := testutil.ToFloat64(env.metrics.HandshakeCounter(metrics.ResultFull)); got != 1 {
		t.Errorf("full handshakes = %v, want 1", got)
	}
}

// dialUDP opens a connected UDP socket to addr, which is the client's local port the
// proxy keys the session on.
func dialUDP(t *testing.T, addr string) *net.UDPConn {
	t.Helper()
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve %q: %v", addr, err)
	}
	c, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		t.Fatalf("dial %q: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// sendAndRead writes one datagram and returns the next datagram back.
func sendAndRead(t *testing.T, c *net.UDPConn, msg []byte) []byte {
	t.Helper()
	if _, err := c.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	return readReply(t, c)
}

// handshake sends the encoded handshake for ticket and returns the answer.
func handshake(t *testing.T, c *net.UDPConn, tok string) []byte {
	t.Helper()
	raw, ok := EncodeHandshake(tok)
	if !ok {
		t.Fatal("ticket too long for a handshake")
	}
	return sendAndRead(t, c, raw)
}

// readReply reads one datagram with a generous deadline so a genuine answer is never
// mistaken for a drop on a slow machine.
func readReply(t *testing.T, c *net.UDPConn) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return append([]byte(nil), buf[:n]...)
}

// startEcho runs a UDP echo server on loopback and returns its address.
func startEcho(t *testing.T) string {
	t.Helper()
	ua, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve echo address: %v", err)
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDPAddrPort(buf[:n], from)
		}
	}()
	return conn.LocalAddr().String()
}

// TestEndToEndSlowAllocatorDoesNotStallForwarding: a handshake that makes the proxy wait
// on the Allocator (here an unknown srv forces a directory refresh against a slow
// Allocator) must not hold up game traffic for sessions that already exist. When
// handshakes ran on the read loop, the echo below waited out the whole slow refresh.
func TestEndToEndSlowAllocatorDoesNotStallForwarding(t *testing.T) {
	pub, priv := mustKey(t)
	echo := startEcho(t)
	fa := newFakeAllocator(t, pub, "test-key-0123456789")
	fa.setServers(serverEntry{ServerID: "gs-1", InternalAddr: echo, State: "busy"})
	clk := newFakeClock(time.Now())
	env := startTestEnv(t, fa, 30*time.Second, 100, clk.Now)
	kid := ticket.Thumbprint(pub)

	c1 := dialUDP(t, env.addr)
	if got := string(handshake(t, c1, issue(t, priv, kid, "gs-1", time.Now().Add(time.Minute)))); got != "OTOK" {
		t.Fatalf("handshake answer = %q, want OTOK", got)
	}

	// Past the directory's refresh rate limit, then make the Allocator slow and send a
	// handshake for a server the proxy doesn't know.
	clk.Advance(2 * time.Second)
	fa.setDelay(1500 * time.Millisecond)
	c2 := dialUDP(t, env.addr)
	if _, err := c2.Write(mustEncode(t, issue(t, priv, kid, "gs-unknown", time.Now().Add(time.Minute)))); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the proxy start the slow refresh

	start := time.Now()
	if got := string(sendAndRead(t, c1, []byte("still here"))); got != "still here" {
		t.Fatalf("forwarded echo = %q, want still here", got)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("forwarding waited %v behind a handshake's Allocator call", elapsed)
	}

	// The slow handshake still gets its answer once the Allocator replies.
	if got := string(readReply(t, c2)); got != "OTNO\x04" {
		t.Fatalf("unknown-server answer = %q, want OTNO 4", got)
	}
}
