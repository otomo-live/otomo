package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/loki"
	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// logsPath is the external path for GET /logs.
const logsPath = "/api/admin/dashboard/logs"

// logsPattern is the ServeMux pattern for logsPath, compared by For while the mux is
// assembled.
const logsPattern = http.MethodGet + " " + logsPath

const (
	// defaultLogLimit is how many lines a request without an explicit limit returns.
	defaultLogLimit = 200
	// maxLogLimit is the hard cap on a single /logs response (DSH-C8).
	maxLogLimit = 1000
	// maxLogRange is the widest window /logs will query. Unlike the series endpoint,
	// which clamps, a wider log window is rejected: a log query scans text, not a
	// pre-aggregated series, so silently widening or narrowing it would surprise the
	// caller reading the response.
	maxLogRange = 7 * 24 * time.Hour
)

// logsResponse is the wire body GET /logs returns: newest first, at most maxLogLimit
// entries. The WebUI parses "entries" (design/01-dashboard.md §4).
type logsResponse struct {
	Entries []logEntry `json:"entries"`
}

// logEntry is one log line on the wire. Fields is omitted entirely when the line was not
// JSON, so a plain-text log does not carry an empty object the UI would render as
// expandable-nothing.
type logEntry struct {
	At      time.Time      `json:"at"`
	Service string         `json:"service"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// logEntries converts the source's lines to the wire shape. It always returns a non-nil
// slice so an empty result serialises as [] rather than null.
func logEntries(lines []source.LogLine) []logEntry {
	out := make([]logEntry, len(lines))
	for i, line := range lines {
		out[i] = logEntryFrom(line)
	}
	return out
}

// logEntryFrom converts one source line to its wire shape.
func logEntryFrom(line source.LogLine) logEntry {
	return logEntry{
		At:      line.At.UTC(),
		Service: line.Service,
		Level:   line.Level,
		Message: line.Message,
		Fields:  line.Fields,
	}
}

// logs answers GET /logs: one Loki query_range, validated and bounded, newest first. Every
// client-chosen value is validated before it reaches Loki: an unknown service or level, a
// malformed request_id, a newline in contains, an inverted or over-wide window and an
// out-of-range limit are all 400 validation_failed; a Loki failure is 502 upstream_error.
func (h *Handlers) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}

	to, ok := parseSeriesTime(q.Get("to"), now)
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "to is not a time")
		return
	}
	if to.After(now) {
		to = now
	}
	fromParam := q.Get("from")
	from := to.Add(-time.Hour)
	if fromParam != "" {
		from, ok = parseSeriesTime(fromParam, time.Time{})
		if !ok {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "from is not a time")
			return
		}
	}
	if !from.Before(to) {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "from must be before to")
		return
	}
	if to.Sub(from) > maxLogRange {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "range must be at most 7 days")
		return
	}

	limit := defaultLogLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLogLimit {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "limit must be between 1 and 1000")
			return
		}
		limit = n
	}

	query := source.LogQuery{
		Service:   q.Get("service"),
		Level:     q.Get("level"),
		Contains:  q.Get("contains"),
		RequestID: q.Get("request_id"),
		From:      from,
		To:        to,
		Limit:     limit,
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

	lines, err := h.Logs.Query(r.Context(), query)
	if err != nil {
		WriteError(w, r, http.StatusBadGateway, "upstream_error", "the log store could not answer")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(logsResponse{Entries: logEntries(lines)})
}

// logServices fetches Loki's service allow-list when the query names a service, and reports
// whether the caller should carry on. A fetch that fails with no last-good set is a 502;
// the response has already been written when false is returned.
func (h *Handlers) logServices(w http.ResponseWriter, r *http.Request, service string) (promql.ServiceSet, bool) {
	if service == "" {
		return nil, true
	}
	if h.LogServices == nil {
		WriteError(w, r, http.StatusBadGateway, "upstream_error", "the log service list is not configured")
		return nil, false
	}
	set, err := h.LogServices(r.Context())
	if err != nil && len(set) == 0 {
		WriteError(w, r, http.StatusBadGateway, "upstream_error", "the log store could not answer")
		return nil, false
	}
	return set, true
}

// logValidationMessage renders a builder error for a human without echoing the value back:
// a validation message is shown in the UI, and repeating a client-supplied string there is
// how an error becomes an injection vector of its own.
func logValidationMessage(err error) string {
	switch {
	case err == nil:
		return "invalid log query"
	case errors.Is(err, loki.ErrUnknownService):
		return "unknown service"
	case errors.Is(err, loki.ErrUnknownLevel):
		return "unknown level"
	case errors.Is(err, loki.ErrInvalidRequestID):
		return "invalid request_id"
	case errors.Is(err, loki.ErrInvalidContains):
		return "contains must not contain a newline"
	default:
		return "invalid log query"
	}
}
