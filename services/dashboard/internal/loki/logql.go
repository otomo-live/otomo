// Package loki implements source.LogSource against Loki's HTTP API.
//
// It is the only place in the Dashboard where a LogQL string is written. Clients never send
// LogQL: the handler validates the small set of query parameters, and Build composes them
// into the stream selector and line filters below, escaping every client-chosen string as a
// double-quoted Go/LogQL literal so a value can only ever be a value
// (design/01-dashboard.md §3, rule 2).
package loki

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// Validation errors returned by Build. The API layer maps every one of them to the same
// 400 validation_failed, but keeping them distinct makes the builder's own tests precise
// and lets a caller tell which parameter was refused.
var (
	ErrUnknownService   = errors.New("loki: unknown service")
	ErrUnknownLevel     = errors.New("loki: unknown level")
	ErrInvalidRequestID = errors.New("loki: invalid request_id")
	ErrInvalidContains  = errors.New("loki: contains must not contain a newline")
)

// serviceRE is the grammar a service label value must satisfy. It is the same DNS-label
// shape every service name on the platform uses; the 32-character cap keeps a label small.
var serviceRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// requestIDRE is the grammar a request_id filter value must satisfy. The Dashboard's own
// services mint request IDs from this alphabet, so anything else is a client error rather
// than a legitimate value that needs escaping.
var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// levels is the closed set of label values the level filter accepts. Loki stores level as a
// label, so an arbitrary value is a query that can never match rather than a useful filter.
var levels = map[string]struct{}{
	"debug": {},
	"info":  {},
	"warn":  {},
	"error": {},
}

// Build composes the LogQL for q against services, the set of service label values Loki
// currently exposes. It is a pure function: it touches no network and has no state, which is
// what makes the injection cases cheap to test exhaustively.
//
// The shape is a stream selector followed by line filters:
//
//	{service="gateway",level="error"} |= "publish" |= "request_id\":\"abc\""
//
// An empty Service selects every stream ({service=~".+"}) instead of rejecting the query,
// which is what the log viewer's "all services" default needs. A request_id is matched as
// the literal substring the JSON line contains (request_id":"<id>"), not as a parsed field,
// because Alloy indexes only service and level and `| json` would make this scan every
// stream.
func Build(q source.LogQuery, services promql.ServiceSet) (string, error) {
	var b strings.Builder

	b.WriteByte('{')
	if q.Service == "" {
		b.WriteString(`service=~".+"`)
	} else {
		if !serviceRE.MatchString(q.Service) || !services.Contains(q.Service) {
			return "", fmt.Errorf("%w: %q", ErrUnknownService, q.Service)
		}
		b.WriteString("service=")
		b.WriteString(strconv.Quote(q.Service))
	}

	if q.Level != "" {
		if _, ok := levels[q.Level]; !ok {
			return "", fmt.Errorf("%w: %q", ErrUnknownLevel, q.Level)
		}
		b.WriteString(",level=")
		b.WriteString(strconv.Quote(q.Level))
	}
	b.WriteByte('}')

	if q.Contains != "" {
		if strings.ContainsAny(q.Contains, "\r\n") {
			return "", ErrInvalidContains
		}
		b.WriteString(" |= ")
		b.WriteString(strconv.Quote(q.Contains))
	}

	if q.RequestID != "" {
		if !requestIDRE.MatchString(q.RequestID) {
			return "", fmt.Errorf("%w: %q", ErrInvalidRequestID, q.RequestID)
		}
		// The decoded filter value is request_id":"<id>", which is the substring the raw
		// JSON line contains. Quoting it is what turns the surrounding quotes and colons
		// into literal bytes rather than LogQL syntax.
		b.WriteString(" |= ")
		b.WriteString(strconv.Quote(`request_id":"` + q.RequestID + `"`))
	}

	return b.String(), nil
}
