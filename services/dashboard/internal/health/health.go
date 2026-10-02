// Package health probes the other Otomo services' /readyz endpoints.
//
// Prometheus's up metric only says that a scrape succeeded; it cannot say why a service
// is not serving. Every Otomo service answers GET /readyz on its internal listener with
// 200 "ok", or 503 and a COM-5 body whose error.message names the failing dependency.
// The Prober turns that into "config: not ready — database unreachable" on the overview,
// and exports it to Prometheus as dashboard_service_up/ready.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// maxBodyBytes caps how much of a /readyz response body the prober reads. A
	// misbehaving service must not be able to make the Dashboard allocate.
	maxBodyBytes = 4 << 10
	// maxReasonBytes caps how much of a non-COM-5 body is quoted as the reason, so a
	// large HTML error page does not become the value of every overview panel.
	maxReasonBytes = 200
	// notProbedYet is the reason before a target's first probe completes.
	notProbedYet = "not probed yet"
	// defaultInterval and defaultTimeout keep a zero-value Prober usable in a test or
	// a half-wired server.
	defaultInterval = 15 * time.Second
	defaultTimeout  = 2 * time.Second
)

// Status is one target's most recent probe result. Up means an HTTP response arrived at
// all; Ready means it was 200. Reason explains a non-ready result in the target's own
// words. LastReadyAt is nil until the target has answered ready at least once.
type Status struct {
	Name        string     `json:"name"`
	Up          bool       `json:"up"`
	Ready       bool       `json:"ready"`
	Reason      string     `json:"reason"`
	LatencyMs   int64      `json:"latency_ms"`
	CheckedAt   time.Time  `json:"-"`
	LastReadyAt *time.Time `json:"last_ready_at"`
}

// MarshalJSON renders checked_at as null until the first probe finishes, rather than as
// Go's zero time, which a UI would show as a check two thousand years ago.
func (s Status) MarshalJSON() ([]byte, error) {
	type plain Status
	var checkedAt *time.Time
	if !s.CheckedAt.IsZero() {
		checkedAt = &s.CheckedAt
	}
	return json.Marshal(struct {
		plain
		CheckedAt *time.Time `json:"checked_at"`
	}{plain(s), checkedAt})
}

// Target is one service the prober polls: the name used in the /services response and
// metric labels, and the internal base URL /readyz is appended to.
type Target struct {
	Name string
	URL  string
}

// Observer is told about every completed probe. The server implements it to feed the
// Prometheus gauges and histogram; leaving it nil keeps the prober pure.
type Observer interface {
	ProbeFinished(service string, up, ready bool, latency time.Duration)
}

// Prober polls all of its targets concurrently. Build one, set Observer if metrics are
// wanted, then Run it; Snapshot is safe to call from any goroutine while it runs.
type Prober struct {
	Targets  []Target
	Interval time.Duration
	Timeout  time.Duration
	Client   *http.Client
	Now      func() time.Time
	Observer Observer

	mu       sync.RWMutex
	statuses map[string]Status
}

// NewProber returns a Prober ready to Run. A nil client is replaced by a plain one; the
// prober never follows redirects regardless of what the caller supplied.
func NewProber(targets []Target, interval, timeout time.Duration, client *http.Client) *Prober {
	return &Prober{Targets: targets, Interval: interval, Timeout: timeout, Client: client}
}

// Run probes every target once immediately, then once per Interval until ctx is done.
// Each round waits for every probe to finish before the next begins, so a target that
// hangs for the whole timeout cannot make rounds overlap.
func (p *Prober) Run(ctx context.Context) {
	p.ensureInit()

	for {
		p.probeAll(ctx)

		timer := time.NewTimer(p.interval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Snapshot returns every target's last result sorted by name, including targets whose
// first probe has not finished yet (they report "not probed yet"). The returned slice and
// its pointed-to times are copies, so a caller may keep them.
func (p *Prober) Snapshot() []Status {
	p.ensureInit()

	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]Status, 0, len(p.Targets))
	for _, t := range p.Targets {
		out = append(out, p.statuses[t.Name])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// probeAll runs one round: every target is probed in its own goroutine and the function
// returns once they have all finished.
func (p *Prober) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, target := range p.Targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			p.probe(ctx, t)
		}(target)
	}
	wg.Wait()
}

// probe performs one GET /readyz and records the result. Any HTTP response means the
// service is up; only 200 means it is ready.
func (p *Prober) probe(ctx context.Context, t Target) {
	timeout := p.timeout()
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := strings.TrimSuffix(t.URL, "/") + "/readyz"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)

	start := time.Now()
	var (
		status Status
	)
	if err != nil {
		status = Status{Name: t.Name, Reason: err.Error()}
	} else {
		resp, err := p.client().Do(req)
		status = Status{Name: t.Name}
		switch {
		case err != nil:
			status.Reason = probeError(err, timeout)
		default:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
			_ = resp.Body.Close()

			status.Up = true
			if resp.StatusCode == http.StatusOK {
				status.Ready = true
			} else {
				status.Reason = failureReason(resp.StatusCode, body)
			}
		}
	}
	latency := time.Since(start)

	status.CheckedAt = p.now()
	status.LatencyMs = latency.Milliseconds()
	p.record(t.Name, status, latency)
}

// record stores one result, carrying LastReadyAt forward across failures, and notifies
// the Observer outside the lock.
func (p *Prober) record(name string, status Status, latency time.Duration) {
	p.mu.Lock()
	if p.statuses == nil {
		p.ensureInitLocked()
	}
	if prev, ok := p.statuses[name]; ok && prev.LastReadyAt != nil {
		status.LastReadyAt = prev.LastReadyAt
	}
	if status.Ready {
		at := status.CheckedAt
		status.LastReadyAt = &at
	}
	p.statuses[name] = status
	p.mu.Unlock()

	if p.Observer != nil {
		p.Observer.ProbeFinished(name, status.Up, status.Ready, latency)
	}
}

// failureReason renders a non-200 response: the COM-5 error.message when the body is one,
// otherwise the first bytes of the body, otherwise the bare status.
func failureReason(code int, body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	if text := strings.TrimSpace(string(body)); text != "" {
		if len(text) > maxReasonBytes {
			// Cut on a rune boundary so the reason stays valid UTF-8.
			cut := maxReasonBytes
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut]
		}
		return text
	}
	return fmt.Sprintf("HTTP %d", code)
}

// probeError is the short transport error a person can act on. A timeout reads the same
// whether it came from the context or the transport; a refused or reset connection is
// named rather than quoted, because the raw error embeds the URL and the port.
func probeError(err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return "timeout after " + timeout.String()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout after " + timeout.String()
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	}

	// Nothing matched: report the innermost error so the reason does not quote the
	// target URL and port back into every overview panel.
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err.Error()
		}
		err = inner
	}
}

// client returns a copy of the configured client with redirects disabled. The contract
// forbids following a redirect, so a caller's own CheckRedirect cannot opt back in.
func (p *Prober) client() *http.Client {
	c := new(http.Client)
	if p.Client != nil {
		c = p.Client
	}
	clone := *c
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

func (p *Prober) interval() time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}
	return defaultInterval
}

func (p *Prober) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return defaultTimeout
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prober) ensureInit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureInitLocked()
}

func (p *Prober) ensureInitLocked() {
	if p.statuses != nil {
		return
	}
	p.statuses = make(map[string]Status, len(p.Targets))
	for _, t := range p.Targets {
		p.statuses[t.Name] = Status{Name: t.Name, Reason: notProbedYet}
	}
}
