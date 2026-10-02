package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/auth"
	"github.com/otomo-live/otomo/services/dashboard/internal/loki"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// logsTailPath is the external path for GET /logs/tail, the SSE endpoint.
const logsTailPath = "/api/admin/dashboard/logs/tail"

// logsTailPattern is the ServeMux pattern for logsTailPath, compared by For while the mux
// is assembled.
const logsTailPattern = http.MethodGet + " " + logsTailPath

const (
	// defaultTailMaxPerUser is DSH-C9's per-staff cap: a person may hold two live tails.
	defaultTailMaxPerUser = 2
	// defaultTailMaxGlobal caps live tails across every staff member, so a page left open
	// in a hundred tabs cannot exhaust the Dashboard's goroutines or Loki's.
	defaultTailMaxGlobal = 10
	// defaultTailKeepAlive is how often a comment is written to a stream that has no
	// lines. Gateway and intermediate proxies drop a quiet connection; the comment is
	// what keeps it open (DSH-C10).
	defaultTailKeepAlive = 15 * time.Second
	// tailMaxDuration is the optional hard cap: a stream closes after an hour and the
	// client reconnects, which bounds how long one connection can hold a Loki poll.
	tailMaxDuration = time.Hour
)

// tailLimiter enforces the concurrent-SSE caps: at most maxPerUser streams for one staff
// subject and at most maxGlobal in total. It is a small type of its own so the counting
// rules can be tested without an HTTP server.
//
// The zero value is not usable; build one with newTailLimiter. Every method is safe for
// concurrent use.
type tailLimiter struct {
	mu         sync.Mutex
	perUser    map[string]int
	global     int
	maxPerUser int
	maxGlobal  int
}

// newTailLimiter returns a limiter with the given caps. A non-positive cap falls back to the
// documented default, so a half-wired caller is bounded rather than unbounded.
func newTailLimiter(maxPerUser, maxGlobal int) *tailLimiter {
	if maxPerUser <= 0 {
		maxPerUser = defaultTailMaxPerUser
	}
	if maxGlobal <= 0 {
		maxGlobal = defaultTailMaxGlobal
	}
	return &tailLimiter{
		perUser:    make(map[string]int),
		maxPerUser: maxPerUser,
		maxGlobal:  maxGlobal,
	}
}

// acquire reserves one slot for subject. It returns a release func and "" on success, or a
// nil release and the reason ("user" or "global") when a cap is reached. The release func is
// idempotent, so a caller may safely defer it and also call it directly.
func (l *tailLimiter) acquire(subject string) (func(), string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.global >= l.maxGlobal {
		return nil, "global"
	}
	if l.perUser[subject] >= l.maxPerUser {
		return nil, "user"
	}

	l.global++
	l.perUser[subject]++

	var once sync.Once
	release := func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.global--
			l.perUser[subject]--
			if l.perUser[subject] <= 0 {
				delete(l.perUser, subject)
			}
		})
	}
	return release, ""
}

// limiter returns the handler's limiter, building the default one (2 per user, 10 global) on
// first use. A test may set tailLimiter first to inject smaller caps.
func (h *Handlers) limiter() *tailLimiter {
	h.tailOnce.Do(func() {
		if h.tailLimiter == nil {
			h.tailLimiter = newTailLimiter(defaultTailMaxPerUser, defaultTailMaxGlobal)
		}
	})
	return h.tailLimiter
}

// logsTail answers GET /logs/tail as a Server-Sent Events stream. Validation and the
// concurrency cap happen before a single stream byte: a refused request is a normal COM-5
// JSON error, which a browser's fetch reader can still parse, rather than an SSE stream that
// opens and immediately closes.
//
// Once open the handler writes ": connected", flushes, and then loops over lines from the
// Loki poll and a keep-alive ticker until the client disconnects, the hard cap fires, or a
// write fails. It clears the listener's write deadline for its duration; without that the
// server-wide WriteTimeout would close every stream.
func (h *Handlers) logsTail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	query := source.LogQuery{
		Service:  q.Get("service"),
		Level:    q.Get("level"),
		Contains: q.Get("contains"),
		From:     now,
		Limit:    defaultLogLimit,
	}

	if h.Logs == nil {
		WriteError(w, r, http.StatusBadGateway, "upstream_error", "the log store is not configured")
		return
	}
	services, ok := h.logServices(w, r, query.Service)
	if !ok {
		return
	}
	if _, err := loki.Build(query, services); err != nil {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", logValidationMessage(err))
		return
	}

	subject := "anonymous"
	if claims := auth.ClaimsFrom(r.Context()); claims != nil && claims.Subject != "" {
		subject = claims.Subject
	}
	release, reason := h.limiter().acquire(subject)
	if reason != "" {
		WriteError(w, r, http.StatusTooManyRequests, "too_many_tails", "too many concurrent log tails")
		return
	}
	defer release()

	// The gauge tracks exactly the tails holding a slot: it moves only after acquire
	// succeeds and is dropped by defer when this handler returns for any reason.
	if h.Observer != nil {
		h.Observer.TailStreams(1)
		defer h.Observer.TailStreams(-1)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")

	rc := http.NewResponseController(w)
	// The public listener's WriteTimeout is sized for small JSON responses; a tail is
	// deliberately long-lived, so this handler opts out of it for its duration.
	_ = rc.SetWriteDeadline(time.Time{})

	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), tailMaxDuration)
	defer cancel()

	out := make(chan source.LogLine, 16)
	go func() {
		defer close(out)
		// Tail returns when ctx ends; its error is not actionable once the stream is
		// open, so it is discarded and the closed channel ends the loop.
		_ = h.Logs.Tail(ctx, query, out)
	}()

	keepAlive := h.TailKeepAlive
	if keepAlive <= 0 {
		keepAlive = defaultTailKeepAlive
	}
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case line, open := <-out:
			if !open {
				return
			}
			body, err := json.Marshal(logEntryFrom(line))
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
