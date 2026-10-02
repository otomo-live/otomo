// Package promql is the Dashboard's query-template catalogue: the single place where a
// PromQL string is written, and the only place a client-chosen service name is ever allowed
// to influence one.
//
// Clients of the Dashboard never send PromQL. They name a template, a service and a window;
// Lookup returns that template and Build substitutes the two values into its stored query
// (design/01-dashboard.md §3, rule 2). The service is validated twice before it gets near the
// query — against the caller's allow-list of live targets and against a conservative label
// grammar — so a service name can never break out of its label value.
package promql

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kind says which query shape a template is valid for. A template may allow both, so the
// values are a bitmask.
type Kind int

const (
	// Instant marks a template that can be run as a point-in-time query.
	Instant Kind = 1 << iota
	// Range marks a template that can be run as a time-series query.
	Range
)

// ErrUnknownTemplate is returned by Lookup for a name that is not in the catalogue.
var ErrUnknownTemplate = errors.New("promql: unknown template")

// ErrUnknownService is returned by Build for a service that is malformed or not in the
// caller's allow-list. It is deliberately one error: which half failed is not information a
// client needs.
var ErrUnknownService = errors.New("promql: unknown service")

// serviceRE is the grammar every service label value must satisfy. It is a DNS label with
// hyphens allowed, which covers the service names the platform actually uses while
// excluding quotes, braces, whitespace and newlines that could change the meaning of a
// query. The 32-character cap keeps a label value small.
var serviceRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Template is one named, parameterized query. Query holds the literal PromQL to send once
// {service} and {window} have been substituted; callers must use Build rather than editing
// the query themselves.
type Template struct {
	Name  string
	Title string
	Unit  string
	Query string
	// Kinds says whether this template is meaningful as an instant query, a range query,
	// or both.
	Kinds Kind
}

// ServiceSet is the set of services a caller is willing to run a template for. It is built
// from Prometheus's own target list (design/01-dashboard.md §3, rule 2), so a typo or a
// service that is not currently scraped is rejected instead of silently returning an empty
// series.
type ServiceSet map[string]struct{}

// NewServiceSet returns a ServiceSet containing names.
func NewServiceSet(names ...string) ServiceSet {
	s := make(ServiceSet, len(names))
	for _, name := range names {
		s[name] = struct{}{}
	}
	return s
}

// Contains reports whether name is in the set. A nil set contains nothing.
func (s ServiceSet) Contains(name string) bool {
	_, ok := s[name]
	return ok
}

// catalogue is the whole query surface. Names are API and must not change without a
// client-visible reason; each Query uses only the two placeholders Build replaces.
var catalogue = []Template{
	{
		Name:  "rps_by_status",
		Title: "Requests per second by status class",
		Unit:  "req/s",
		Kinds: Instant | Range,
		Query: `sum by (class) (label_replace(rate({__name__=~".+_http_requests_total",service="{service}"}[{window}]), "class", "${1}xx", "status", "([0-9]).."))`,
	},
	{
		Name:  "error_ratio",
		Title: "5xx error ratio",
		Unit:  "ratio",
		Kinds: Instant | Range,
		Query: `sum(rate({__name__=~".+_http_requests_total",service="{service}",status=~"5.."}[{window}])) / clamp_min(sum(rate({__name__=~".+_http_requests_total",service="{service}"}[{window}])), 1e-9)`,
	},
	{
		Name:  "latency_p50",
		Title: "Latency p50",
		Unit:  "s",
		Kinds: Instant | Range,
		Query: `histogram_quantile(0.50, sum by (le) (rate({__name__=~".+_http_request_duration_seconds_bucket",service="{service}"}[{window}])))`,
	},
	{
		Name:  "latency_p95",
		Title: "Latency p95",
		Unit:  "s",
		Kinds: Instant | Range,
		Query: `histogram_quantile(0.95, sum by (le) (rate({__name__=~".+_http_request_duration_seconds_bucket",service="{service}"}[{window}])))`,
	},
	{
		Name:  "latency_p99",
		Title: "Latency p99",
		Unit:  "s",
		Kinds: Instant | Range,
		Query: `histogram_quantile(0.99, sum by (le) (rate({__name__=~".+_http_request_duration_seconds_bucket",service="{service}"}[{window}])))`,
	},
	{
		Name:  "cpu",
		Title: "CPU usage",
		Unit:  "cores",
		Kinds: Instant | Range,
		Query: `sum(rate(container_cpu_usage_seconds_total{container_label_com_docker_compose_service="{service}"}[{window}]))`,
	},
	{
		Name:  "memory",
		Title: "Memory working set",
		Unit:  "bytes",
		Kinds: Instant | Range,
		Query: `sum(container_memory_working_set_bytes{container_label_com_docker_compose_service="{service}"})`,
	},
	{
		Name:  "goroutines",
		Title: "Goroutines",
		Unit:  "count",
		Kinds: Instant | Range,
		Query: `sum(go_goroutines{service="{service}"})`,
	},
	{
		Name:  "heap_inuse",
		Title: "Heap in use",
		Unit:  "bytes",
		Kinds: Instant | Range,
		Query: `sum(go_memstats_heap_inuse_bytes{service="{service}"})`,
	},
	{
		Name: "gc_pause_max",
		// client_golang's default Go collector exposes go_gc_duration_seconds as a
		// summary, so quantile="1" is the maximum stop-the-world pause it records. A
		// true p99 would need the histogram, which the collector does not publish.
		Title: "GC pause max (summary quantile; p99 unavailable)",
		Unit:  "s",
		Kinds: Instant | Range,
		Query: `max(go_gc_duration_seconds{service="{service}",quantile="1"})`,
	},
}

// byName indexes catalogue, filled once at init.
var byName = func() map[string]Template {
	m := make(map[string]Template, len(catalogue))
	for _, t := range catalogue {
		m[t.Name] = t
	}
	return m
}()

// names is the sorted catalogue keys, computed once so Names stays allocation-free and
// deterministic.
var names = func() []string {
	out := make([]string, 0, len(byName))
	for name := range byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}()

// minWindow is the smallest rate window a template will accept. Below a minute, rate() has
// too few samples to be meaningful and a browser could use a tiny window to make Prometheus
// scan the same range over and over.
const minWindow = time.Minute

// Lookup returns the template registered under name, or ErrUnknownTemplate.
func Lookup(name string) (Template, error) {
	t, ok := byName[name]
	if !ok {
		return Template{}, fmt.Errorf("%w: %q", ErrUnknownTemplate, name)
	}
	return t, nil
}

// Names returns every template name in ascending order. It returns a copy, so a caller
// cannot mutate the catalogue.
func Names() []string {
	out := make([]string, len(names))
	copy(out, names)
	return out
}

// Build renders t for one service and window. The service is checked against allowed and
// against the label grammar before any substitution happens, so a value that fails never
// reaches the query string; window is floored to whole seconds and to minWindow.
func (t Template) Build(service string, window time.Duration, allowed ServiceSet) (string, error) {
	if !serviceRE.MatchString(service) || !allowed.Contains(service) {
		return "", fmt.Errorf("%w: %q", ErrUnknownService, service)
	}

	secs := int64(window / time.Second)
	if secs < int64(minWindow/time.Second) {
		secs = int64(minWindow / time.Second)
	}

	q := strings.ReplaceAll(t.Query, "{service}", service)
	q = strings.ReplaceAll(q, "{window}", fmt.Sprintf("%ds", secs))
	return q, nil
}
