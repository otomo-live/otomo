// Handlers is the seam between the route table and the code behind each route: Routes()
// states which paths exist, Handlers states which of them are implemented. A route with
// no entry here stays behind NotImplemented, so the 501 placeholders shrink one ticket at
// a time without the route table moving.

package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/auditsrc"
	"github.com/otomo-live/otomo/services/dashboard/internal/cache"
	"github.com/otomo-live/otomo/services/dashboard/internal/health"
	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// Observer is the handler side of the instrumentation seam: the events this package emits
// that are not tied to one transport. The server layer implements it, so this package never
// imports a metrics library. Nil is valid and instruments nothing.
type Observer interface {
	// CacheRequest records one response-cache lookup: endpoint is "overview" or
	// "series", result is hit, miss or shared.
	CacheRequest(endpoint, result string)
	// OverviewDegraded records one card degraded by a failed or empty overview query.
	OverviewDegraded(card string)
	// TailStreams adjusts the open-SSE-tail gauge by delta.
	TailStreams(delta int)
}

// Handlers carries everything a route handler needs from outside this package. Every
// field may be nil in a half-wired server: For still answers every route, with
// NotImplemented, so a missing dependency fails closed rather than panicking.
type Handlers struct {
	// Services returns the prober's snapshot. When it is nil, /services answers an empty
	// list rather than a 501 so a Dashboard with no configured targets is a valid state.
	Services func() []health.Status

	// Observer receives the handler-level instrumentation events. Nil instruments
	// nothing; the server wires itself here after construction.
	Observer Observer

	// Metrics is the Prometheus query surface the overview batch runs against. A nil
	// source degrades every card rather than failing the response.
	Metrics source.MetricsSource

	// AllowList returns the services Prometheus currently scrapes. It is the Prometheus
	// half of the overview's service union; a failure simply leaves that half empty.
	AllowList func(ctx context.Context) (promql.ServiceSet, error)

	// Logs is the Loki query surface behind GET /logs and GET /logs/tail. A nil source
	// leaves both answering 502 rather than a 501.
	Logs source.LogSource

	// Audit is the two upstream audit feeds behind GET /audit, in merge tie-break order.
	// A nil or short list degrades the missing feed rather than failing the route; the
	// two clients forward the caller's own bearer token.
	Audit []*auditsrc.Client

	// LogServices returns the service label values Loki currently exposes. It is the
	// log half of the service allow-list: a service not in it is rejected before any
	// LogQL is built. A failure that still carries a last-good set keeps serving.
	LogServices func(ctx context.Context) (promql.ServiceSet, error)

	// TailKeepAlive overrides how often the tail handler emits an SSE keep-alive
	// comment. Nil/zero means the default. It exists so a test can observe the
	// keep-alive without waiting 15 seconds.
	TailKeepAlive time.Duration

	// Now is the clock the overview cache key and generated_at are taken from. Nil means
	// time.Now; tests inject a clock so buckets can be rolled without sleeping.
	Now func() time.Time

	cacheOnce sync.Once
	cache     *cache.Cache

	tailOnce    sync.Once
	tailLimiter *tailLimiter
}

// For returns the handler for rt's pattern, or NotImplemented when the pattern has no
// implementation yet. A nil *Handlers is valid and returns NotImplemented for every
// route, which keeps the pre-implementation 501 assertions meaningful.
func (h *Handlers) For(rt Route) http.Handler {
	if h != nil {
		switch rt.Pattern() {
		case overviewPattern:
			return http.HandlerFunc(h.overview)
		case servicesPattern:
			return http.HandlerFunc(h.listServices)
		case seriesPattern:
			return http.HandlerFunc(h.series)
		case logsPattern:
			return http.HandlerFunc(h.logs)
		case logsTailPattern:
			return http.HandlerFunc(h.logsTail)
		case auditPattern:
			return http.HandlerFunc(h.audit)
		}
	}
	return http.HandlerFunc(NotImplemented)
}
