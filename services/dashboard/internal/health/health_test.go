package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// startRun runs p on its own context and stops it when the test ends, so a failing
// assertion never leaves a prober polling in the background.
func startRun(t *testing.T, p *Prober) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after its context was cancelled")
		}
	})
}

// waitStatus polls Snapshot until name satisfies ok, or fails the test at the deadline.
// Every wait is bounded; no test sleeps on an assumption.
func waitStatus(t *testing.T, p *Prober, name string, ok func(Status) bool) Status {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, s := range p.Snapshot() {
			if s.Name == name && ok(s) {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("target %q never reached the expected state (last: %+v)", name, snapshotOf(p, name))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func snapshotOf(p *Prober, name string) Status {
	for _, s := range p.Snapshot() {
		if s.Name == name {
			return s
		}
	}
	return Status{}
}

// TestSnapshotBeforeFirstProbe is the state the overview shows for the first moments after
// start-up: the target exists, and the reason says so rather than implying it is down.
func TestSnapshotBeforeFirstProbe(t *testing.T) {
	p := NewProber([]Target{{Name: "auth", URL: "http://auth:9090"}}, time.Hour, time.Second, nil)

	got := p.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot() = %+v, want one status", got)
	}
	s := got[0]
	if s.Name != "auth" || s.Up || s.Ready {
		t.Errorf("initial status = %+v, want not up and not ready", s)
	}
	if s.Reason != notProbedYet {
		t.Errorf("Reason = %q, want %q", s.Reason, notProbedYet)
	}
	if s.LastReadyAt != nil || !s.CheckedAt.IsZero() {
		t.Errorf("initial status has probe data: %+v", s)
	}
}

func TestSnapshotIsSortedByName(t *testing.T) {
	p := NewProber([]Target{
		{Name: "patch", URL: "http://patch:9090"},
		{Name: "auth", URL: "http://auth:9090"},
		{Name: "config", URL: "http://config:9090"},
	}, time.Hour, time.Second, nil)

	got := p.Snapshot()
	want := []string{"auth", "config", "patch"}
	if len(got) != len(want) {
		t.Fatalf("Snapshot() = %+v, want %d", got, len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("Snapshot()[%d].Name = %q, want %q", i, got[i].Name, name)
		}
	}
}

func TestProbeReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	p := NewProber([]Target{{Name: "auth", URL: srv.URL}}, time.Hour, time.Second, nil)
	startRun(t, p)

	s := waitStatus(t, p, "auth", func(s Status) bool { return s.Ready })
	if !s.Up {
		t.Errorf("Up = false for a 200 response")
	}
	if s.Reason != "" {
		t.Errorf("Reason = %q, want empty for ready", s.Reason)
	}
	if s.CheckedAt.IsZero() {
		t.Error("CheckedAt is zero")
	}
	if s.LastReadyAt == nil {
		t.Error("LastReadyAt is nil after a ready probe")
	}
}

func TestProbeNotReadyCOM5Body(t *testing.T) {
	const message = "not_ready: database: dial tcp 10.0.0.5:5432: connect: connection refused"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"not_ready","message":"`+message+`","request_id":"r1"}}`)
	}))
	t.Cleanup(srv.Close)

	p := NewProber([]Target{{Name: "config", URL: srv.URL}}, time.Hour, time.Second, nil)
	startRun(t, p)

	s := waitStatus(t, p, "config", func(s Status) bool { return !s.CheckedAt.IsZero() && !s.Ready })
	if !s.Up {
		t.Error("Up = false although the target answered 503")
	}
	if s.Reason != message {
		t.Errorf("Reason = %q, want the COM-5 message %q", s.Reason, message)
	}
	if s.LastReadyAt != nil {
		t.Error("LastReadyAt is set for a target that never answered ready")
	}
}

func TestProbeNotReadyPlainBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "  manifests not loaded\n")
	}))
	t.Cleanup(srv.Close)

	p := NewProber([]Target{{Name: "config", URL: srv.URL}}, time.Hour, time.Second, nil)
	startRun(t, p)

	s := waitStatus(t, p, "config", func(s Status) bool { return !s.CheckedAt.IsZero() && !s.Ready })
	if s.Reason != "manifests not loaded" {
		t.Errorf("Reason = %q, want the body text", s.Reason)
	}
}

func TestProbeConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p := NewProber([]Target{{Name: "auth", URL: "http://" + addr}}, time.Hour, time.Second, nil)
	startRun(t, p)

	s := waitStatus(t, p, "auth", func(s Status) bool { return !s.CheckedAt.IsZero() })
	if s.Up || s.Ready {
		t.Errorf("status = %+v, want down", s)
	}
	// A port closed just before the dial normally refuses, but a reused ephemeral port
	// can answer with a reset; both are the same "not reachable" to a reader.
	switch s.Reason {
	case "connection refused", "connection reset":
	default:
		t.Errorf("Reason = %q, want a connection error", s.Reason)
	}
}

// TestProbeError pins the short, actionable strings the overview shows for transport
// failures, independent of which kernel error a given machine produces.
func TestProbeError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "timeout after 2s"},
		{
			"refused",
			&url.Error{Op: "Get", URL: "http://auth:9090", Err: fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED)},
			"connection refused",
		},
		{"reset", fmt.Errorf("read: %w", syscall.ECONNRESET), "connection reset"},
		{
			"other unwraps to the innermost error",
			&url.Error{Op: "Get", URL: "http://auth:9090", Err: errors.New("some failure")},
			"some failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := probeError(tt.err, 2*time.Second); got != tt.want {
				t.Errorf("probeError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProbeTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	p := NewProber([]Target{{Name: "auth", URL: srv.URL}}, time.Hour, 20*time.Millisecond, nil)
	startRun(t, p)

	s := waitStatus(t, p, "auth", func(s Status) bool { return !s.CheckedAt.IsZero() })
	if s.Up || s.Ready {
		t.Errorf("status = %+v, want down", s)
	}
	if !strings.HasPrefix(s.Reason, "timeout after") {
		t.Errorf("Reason = %q, want a timeout message", s.Reason)
	}
}

func TestProbeDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dest.Close)

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)

	p := NewProber([]Target{{Name: "auth", URL: redirect.URL}}, time.Hour, time.Second, nil)
	startRun(t, p)

	s := waitStatus(t, p, "auth", func(s Status) bool { return s.Up })
	if s.Ready {
		t.Errorf("a 302 was treated as ready")
	}
	if followed.Load() != 0 {
		t.Errorf("the redirect was followed %d times", followed.Load())
	}
}

// countingBody records how many bytes the prober actually read, so the 4 KiB cap is
// asserted directly instead of inferred from the reason length.
type countingBody struct {
	r io.Reader
	n *int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	atomic.AddInt64(c.n, int64(n))
	return n, err
}

func (*countingBody) Close() error { return nil }

type countingTransport struct {
	body string
	read *int64
}

func (c countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Status:     "503 Service Unavailable",
		Header:     make(http.Header),
		Body:       &countingBody{r: strings.NewReader(c.body), n: c.read},
		Request:    req,
	}, nil
}

func TestProbeTruncatesLargeBody(t *testing.T) {
	var read int64
	p := NewProber([]Target{{Name: "auth", URL: "http://auth:9090"}}, time.Hour, time.Second,
		&http.Client{Transport: countingTransport{body: strings.Repeat("x", 32<<10), read: &read}})
	startRun(t, p)

	s := waitStatus(t, p, "auth", func(s Status) bool { return !s.CheckedAt.IsZero() })
	if read > maxBodyBytes {
		t.Errorf("the prober read %d body bytes, want at most %d", read, maxBodyBytes)
	}
	if len(s.Reason) > maxReasonBytes {
		t.Errorf("Reason is %d bytes, want at most %d", len(s.Reason), maxReasonBytes)
	}
}

func TestProbeRecoversAndSetsLastReadyAt(t *testing.T) {
	var ready atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "manifests not loaded")
	}))
	t.Cleanup(srv.Close)

	p := NewProber([]Target{{Name: "config", URL: srv.URL}}, 20*time.Millisecond, time.Second, nil)
	startRun(t, p)

	down := waitStatus(t, p, "config", func(s Status) bool { return !s.CheckedAt.IsZero() && !s.Ready })
	if down.LastReadyAt != nil {
		t.Fatalf("LastReadyAt = %v before the target ever answered ready", down.LastReadyAt)
	}

	ready.Store(true)
	up := waitStatus(t, p, "config", func(s Status) bool { return s.Ready })
	if up.LastReadyAt == nil {
		t.Error("LastReadyAt is nil after recovery")
	}
}

// TestProbesRunConcurrently is the property that keeps a round shorter than the sum of
// its targets' timeouts: five targets that each take 300ms must finish a round in well
// under a second, which is only true if they are probed in parallel.
func TestProbesRunConcurrently(t *testing.T) {
	var targets []Target
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		targets = append(targets, Target{Name: name, URL: srv.URL})
	}

	p := NewProber(targets, time.Hour, 2*time.Second, nil)
	start := time.Now()
	startRun(t, p)

	for _, target := range targets {
		waitStatus(t, p, target.Name, func(s Status) bool { return s.Ready })
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Errorf("one round took %s, want under 1s for five 300ms probes", elapsed)
	}
}

func TestStatusJSONCheckedAtNullBeforeFirstProbe(t *testing.T) {
	b, err := json.Marshal(Status{Name: "config", Reason: notProbedYet})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"config","up":false,"ready":false,"reason":"not probed yet","latency_ms":0,"last_ready_at":null,"checked_at":null}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	b, _ = json.Marshal(Status{Name: "config", CheckedAt: at})
	if !strings.Contains(string(b), `"checked_at":"2026-09-26T12:00:00Z"`) {
		t.Fatalf("checked_at not rendered: %s", b)
	}
}

func TestFailureReasonTruncatesOnRuneBoundary(t *testing.T) {
	body := []byte(strings.Repeat("a", maxReasonBytes-1) + "é" + "tail")
	got := failureReason(503, body)
	if !utf8.ValidString(got) || len(got) != maxReasonBytes-1 {
		t.Fatalf("reason = %q (len %d), want %d valid bytes", got, len(got), maxReasonBytes-1)
	}
}
