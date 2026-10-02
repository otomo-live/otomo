package ratelimit

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/otomo-live/otomo/services/gateway_dev/internal/apierr"
)

type entry struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64 // UnixNano
}

// Limiter is a per-IP token-bucket rate limiter backed by a sync.Map. A
// background sweeper (StartSweeper) evicts entries idle longer than the
// configured maximum idle age.
//
// Known M1 limitation: in-memory per process. If either Gateway is scaled to
// multiple replicas, per-IP limits need to move to Valkey (INCR + EXPIRE).
type Limiter struct {
	rps   rate.Limit
	burst int
	ips   sync.Map // map[string]*entry

	// TODO: TLS termination question (techspec §1) is unresolved. RemoteAddr
	// is the only trustworthy source until that is settled. Do not trust
	// X-Forwarded-For.
}

// New creates a rate limiter with the given requests-per-second and burst.
func New(rps int, burst int) *Limiter {
	return &Limiter{
		rps:   rate.Limit(rps),
		burst: burst,
	}
}

// StartSweeper starts a background goroutine that evicts entries idle longer
// than maxIdleAge. It ticks every sweepInterval and stops when ctx is cancelled.
func (l *Limiter) StartSweeper(ctx context.Context, sweepInterval, maxIdleAge time.Duration) {
	go func() {
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				l.ips.Range(func(key, value any) bool {
					e := value.(*entry)
					if now.Sub(time.Unix(0, e.lastSeen.Load())) > maxIdleAge {
						l.ips.Delete(key)
					}
					return true
				})
			}
		}
	}()
}

// Wrap returns middleware that enforces the rate limit on RemoteAddr (port
// stripped). 429 responses go through apierr in the COM-5 shape.
func (l *Limiter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		lim := l.limiterFor(ip)
		if !lim.Allow() {
			apierr.WriteError(w, r, http.StatusTooManyRequests,
				"rate_limit_exceeded", "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *Limiter) limiterFor(ip string) *rate.Limiter {
	now := time.Now().UnixNano()
	if v, ok := l.ips.Load(ip); ok {
		e := v.(*entry)
		e.lastSeen.Store(now)
		return e.limiter
	}
	lim := rate.NewLimiter(l.rps, l.burst)
	e := &entry{limiter: lim}
	e.lastSeen.Store(now)
	actual, _ := l.ips.LoadOrStore(ip, e)
	return actual.(*entry).limiter
}

// clientIP extracts the IP from RemoteAddr with the port stripped.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
