package promql

// OverviewScope says how one overview query's result is keyed: per service, or as a single
// host/global value.
type OverviewScope int

const (
	// OverviewService marks a query whose vector is grouped by the service label. Each
	// series fills one field of one service card.
	OverviewService OverviewScope = iota
	// OverviewHost marks an aggregate query whose single value fills one host or global
	// card.
	OverviewHost
)

// OverviewQuery is one card in GET /overview's fixed batch. Unlike Template it has no
// parameters: it aggregates across every service, or over the whole host, in one query.
// Query is the literal PromQL, and it must never contain a placeholder.
type OverviewQuery struct {
	// Name is the response field the query fills, e.g. "p95_ms".
	Name string
	// Card is the identifier reported in the response's degraded list when this query
	// fails, e.g. "services.p95_ms".
	Card string
	// Scope says whether the result is keyed by service or is one host value.
	Scope OverviewScope
	// Query is the literal PromQL sent to Prometheus.
	Query string
}

// overviewBatch is the whole GET /overview surface, in the order the cards render. It is
// the single place these queries are written; the handler only runs them and maps labels to
// fields. Cards are named after the response fields, and the degraded identifier adds the
// card's group so a person reading the list knows which strip is missing.
var overviewBatch = []OverviewQuery{
	{
		Name:  "up",
		Card:  "services.up",
		Scope: OverviewService,
		Query: `max by (service) (up{service!=""})`,
	},
	{
		Name:  "rps",
		Card:  "services.rps",
		Scope: OverviewService,
		Query: `sum by (service) (rate({__name__=~".+_http_requests_total",service!=""}[5m]))`,
	},
	{
		Name:  "error_ratio",
		Card:  "services.error_ratio",
		Scope: OverviewService,
		// clamp_min keeps a service with no traffic this window at 0 rather than
		// dividing by zero, which Prometheus renders as +Inf and the card as a broken
		// bar.
		Query: `sum by (service) (rate({__name__=~".+_http_requests_total",service!="",status=~"5.."}[5m])) / clamp_min(sum by (service) (rate({__name__=~".+_http_requests_total",service!=""}[5m])), 1e-9)`,
	},
	{
		Name:  "p95_ms",
		Card:  "services.p95_ms",
		Scope: OverviewService,
		Query: `1000 * histogram_quantile(0.95, sum by (service, le) (rate({__name__=~".+_http_request_duration_seconds_bucket",service!=""}[5m])))`,
	},
	{
		Name:  "version",
		Card:  "services.version",
		Scope: OverviewService,
		// max by (service, version) keeps the version label in the result; the handler
		// reads that label rather than the value.
		Query: `max by (service, version) ({__name__=~".+_build_info",service!=""})`,
	},
	{
		Name:  "cpu_ratio",
		Card:  "host.cpu",
		Scope: OverviewHost,
		Query: `1 - avg(rate(node_cpu_seconds_total{mode="idle"}[1m]))`,
	},
	{
		Name:  "mem_ratio",
		Card:  "host.mem",
		Scope: OverviewHost,
		Query: `1 - sum(node_memory_MemAvailable_bytes) / sum(node_memory_MemTotal_bytes)`,
	},
	{
		Name:  "disk_ratio",
		Card:  "host.disk",
		Scope: OverviewHost,
		Query: `1 - sum(node_filesystem_avail_bytes{mountpoint="/"}) / sum(node_filesystem_size_bytes{mountpoint="/"})`,
	},
	{
		Name:  "online_players",
		Card:  "online_players",
		Scope: OverviewHost,
		// No service exports this yet, so an empty result is expected. The handler
		// treats a host card with no value as degraded, which is exactly what an
		// unpopulated online-player card should be.
		Query: `sum(otomo_online_players)`,
	},
}

// Overview returns a copy of the overview query batch, so a caller cannot mutate the
// catalogue for another request.
func Overview() []OverviewQuery {
	out := make([]OverviewQuery, len(overviewBatch))
	copy(out, overviewBatch)
	return out
}
