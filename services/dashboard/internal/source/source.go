// Package source defines the Dashboard's two query surfaces: metrics from Prometheus and
// logs from Loki.
//
// It contains interfaces only. The Dashboard does not store or index anything — it is a
// thin, access-controlled query layer in front of tools that do (design/01-dashboard.md §1),
// and this package is the seam where the HTTP handlers meet those tools. The transports
// implement the interfaces: DSH-C4/C5/C6 for MetricsSource and DSH-C8/C9 for LogSource.
// Keeping them here, apart from the transports that implement them, means a handler can be
// tested against a fake without a Prometheus or Loki process and a client can be tested
// without a handler.
package source

import (
	"context"
	"time"
)

// Sample is one point on a time series. It exists for the small cases that do not need the
// columnar shape Range returns; the charts themselves use Range's columnar form.
type Sample struct {
	T int64
	V float64
}

// MetricsSource is the Prometheus query surface. Every method takes ready-made PromQL
// built from the named template catalog — never a string a client supplied — because the
// Dashboard is the only place allowed to compose a query
// (design/01-dashboard.md §3, rule 2).
type MetricsSource interface {
	// Instant runs one instant query and returns its value per result series, keyed by the
	// series' label set rendered into a stable string. A query that matches nothing returns
	// an empty map, not an error.
	Instant(ctx context.Context, promql string) (map[string]float64, error)

	// Range runs one range query and returns the columnar shape the WebUI's uPlot consumes
	// directly (design/01-dashboard.md §4): one shared timestamp array and one value slice per
	// series, keyed the same way as Instant's map. Every value slice is the same length as
	// ts, with gaps filled so the arrays stay aligned.
	Range(ctx context.Context, promql string, from, to time.Time, step time.Duration) (ts []int64, series map[string][]float64, err error)

	// Targets returns the currently configured scrape targets and their health, which is
	// what GET /services reports.
	Targets(ctx context.Context) ([]Target, error)

	// Alerts returns the alert rule instances Prometheus is tracking, both firing and
	// pending. The overview filters to firing itself; state is part of the result rather
	// than a parameter so one call can serve either view later.
	Alerts(ctx context.Context) ([]Alert, error)
}

// Alert is one Prometheus alert rule instance: the labels and annotations a rule carries,
// flattened to the handful of fields the Dashboard renders. An unknown label or annotation
// is ignored rather than failing the decode, so a rule gaining fields does not break the
// page. ActiveAt is the timestamp as Prometheus sends it, kept as text.
type Alert struct {
	Name     string
	Severity string
	Service  string
	Summary  string
	State    string
	ActiveAt string
}

// Target is one Prometheus scrape target. The zero value means "unknown"; Up is only
// meaningful once the target has been scraped at least once, which LastScrape.IsZero
// distinguishes.
type Target struct {
	Service     string
	Up          bool
	LastScrape  time.Time
	ScrapeError string
}

// LogLine is one log record as Loki returns it. Fields carries the parsed JSON object the
// log line's message was, when it was JSON; it is nil for plain-text lines.
type LogLine struct {
	At      time.Time
	Service string
	Level   string
	Message string
	Fields  map[string]any
}

// LogSource is the Loki query surface.
type LogSource interface {
	// Query returns matching lines newest first, at most q.Limit of them. It is not a
	// stream: it returns once the upstream has answered or the context ends.
	Query(ctx context.Context, q LogQuery) ([]LogLine, error)

	// Tail streams new lines as they arrive until ctx is cancelled, sending each one on out
	// and returning when the stream ends. The caller owns out and closes it; Tail must not.
	// A send that blocks because the reader has stopped is unblocked by ctx.
	Tail(ctx context.Context, q LogQuery, out chan<- LogLine) error
}

// LogQuery is the validated, parameterized form of a log search. Every field is a value
// the caller chose from a fixed set or a plain string the transport escapes before it
// reaches LogQL; none of them is concatenated into a query by the handler.
type LogQuery struct {
	Service   string
	Level     string
	Contains  string
	RequestID string
	From      time.Time
	To        time.Time
	Limit     int
}
