package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/prometheus"
	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
)

// seriesPath is the external path for GET /services/{name}/series. The wildcard is part of
// the path the route table registers, so it is stated once here and read back from the
// request by the handler.
const seriesPath = "/api/admin/dashboard/services/{name}/series"

// seriesPattern is the ServeMux pattern for seriesPath, compared by For while the mux is
// assembled.
const seriesPattern = http.MethodGet + " " + seriesPath

const (
	// minSeriesStep is the smallest step a caller may ask for. Below it, rate() has too few
	// samples over the window and a browser could make Prometheus re-scan the same range.
	minSeriesStep = 15 * time.Second
	// maxSeriesPoints caps how many points one series may carry. The step is chosen so a
	// response can never exceed it: a 7-day window lands on a ~404 s step.
	maxSeriesPoints = 1500
	// maxSeriesRange is the widest window the endpoint will query. A 30-day request is
	// clamped to this, and the response says so.
	maxSeriesRange = 7 * 24 * time.Hour
)

// seriesResponse is the columnar body the WebUI's uPlot consumes (design/01-dashboard.md §4).
// from/to/step are whole unix seconds; t is the shared timestamp grid and each series'
// values array is the same length, with gaps and NaN rendered as JSON null.
type seriesResponse struct {
	Metric  string         `json:"metric"`
	Title   string         `json:"title"`
	Unit    string         `json:"unit"`
	Service string         `json:"service"`
	From    int64          `json:"from"`
	To      int64          `json:"to"`
	Step    int64          `json:"step"`
	Clamped bool           `json:"clamped"`
	T       []int64        `json:"t"`
	Series  []seriesValues `json:"series"`
}

// seriesValues is one named line on the chart.
type seriesValues struct {
	Name   string     `json:"name"`
	Values []nullable `json:"values"`
}

// nullable is a float64 that marshals non-finite values — NaN and ±Inf, which JSON cannot
// carry — as null, so a gap in Prometheus's data is a gap in the chart rather than a failure
// to serialise the whole response.
type nullable float64

// MarshalJSON renders a finite value as its shortest JSON number and anything non-finite as
// null.
func (n nullable) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, f, 'g', -1, 64), nil
}

// series answers GET /services/{name}/series. It validates every client-chosen value before
// it reaches Prometheus: the metric must name a catalogue template, the service must pass
// the allow-list and the label grammar, and the range and step must parse. The response is
// cached like the overview's, keyed by the canonical (aligned) parameters.
func (h *Handlers) series(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	tmpl, err := promql.Lookup(q.Get("metric"))
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, "unknown_metric", "unknown metric template")
		return
	}

	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}

	to, ok := parseSeriesTime(q.Get("to"), now)
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "to is not a time")
		return
	}
	fromParam := q.Get("from")
	if fromParam == "" {
		to = clampFuture(to, now)
		fromParam = strconv.FormatInt(to.Add(-time.Hour).Unix(), 10)
	}
	from, ok := parseSeriesTime(fromParam, time.Time{})
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "from is not a time")
		return
	}
	to = clampFuture(to, now)

	if !from.Before(to) {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "from must be before to")
		return
	}

	clamped := false
	if to.Sub(from) > maxSeriesRange {
		from = to.Add(-maxSeriesRange)
		clamped = true
	}

	requested, ok := parseSeriesStep(q.Get("step"))
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "step is not a duration")
		return
	}
	step := seriesStep(to.Sub(from), requested)
	from, step = fitSeriesPoints(from, to, step)

	if h.Metrics == nil {
		WriteError(w, r, http.StatusBadGateway, "upstream_error", "the metrics store is not configured")
		return
	}

	if h.AllowList == nil {
		WriteError(w, r, http.StatusServiceUnavailable, "upstream_unavailable", "the service list is unavailable")
		return
	}
	allowed, allowErr := h.AllowList(r.Context())
	if allowErr != nil && len(allowed) == 0 {
		WriteError(w, r, http.StatusServiceUnavailable, "upstream_unavailable", "the service list is unavailable")
		return
	}

	window := 4 * step
	if window < time.Minute {
		window = time.Minute
	}
	query, err := tmpl.Build(r.PathValue("name"), window, allowed)
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, "unknown_service", "unknown service")
		return
	}

	service := r.PathValue("name")
	stepSec := int64(step / time.Second)
	key := seriesPattern + "|" + service + "|" + tmpl.Name + "|" +
		strconv.FormatInt(from.Unix(), 10) + "|" + strconv.FormatInt(to.Unix(), 10) + "|" +
		strconv.FormatInt(stepSec, 10)

	body, err := h.responseCache().Do(r.Context(), cacheEndpointSeries, key, func(ctx context.Context) ([]byte, error) {
		ts, series, err := h.Metrics.Range(ctx, query, from, to, step)
		if err != nil {
			return nil, err
		}
		return json.Marshal(buildSeriesResponse(tmpl, service, from, to, step, clamped, ts, series))
	})
	if err != nil {
		WriteError(w, r, http.StatusBadGateway, "upstream_error", "the metrics store could not answer")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// The browser must not cache the chart; this service's bounded 10 s cache is the only
	// one and it is what absorbs a page polling every few seconds.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// buildSeriesResponse converts the source's columnar map into the wire shape: one named,
// name-sorted series per key, NaN kept as null.
func buildSeriesResponse(tmpl promql.Template, service string, from, to time.Time, step time.Duration, clamped bool, ts []int64, in map[string][]float64) seriesResponse {
	if ts == nil {
		ts = []int64{}
	}
	out := make([]seriesValues, 0, len(in))
	for key, vals := range in {
		values := make([]nullable, len(vals))
		for i, v := range vals {
			values[i] = nullable(v)
		}
		out = append(out, seriesValues{Name: seriesName(key, tmpl.Name), Values: values})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return seriesResponse{
		Metric:  tmpl.Name,
		Title:   tmpl.Title,
		Unit:    tmpl.Unit,
		Service: service,
		From:    from.Unix(),
		To:      to.Unix(),
		Step:    int64(step / time.Second),
		Clamped: clamped,
		T:       ts,
		Series:  out,
	}
}

// seriesName picks a human label for a series. A series with exactly one label is named by
// that label's value (rps_by_status's "2xx"); a series with several is named by its rendered
// key; a single unlabelled series is named after the template.
func seriesName(key, fallback string) string {
	labels := prometheus.Labels(key)
	switch len(labels) {
	case 0:
		return fallback
	case 1:
		for _, v := range labels {
			return v
		}
	}
	return key
}

// parseSeriesTime accepts a whole number of unix seconds or an RFC3339 timestamp. An empty
// string yields def; anything else yields ok == false.
func parseSeriesTime(s string, def time.Time) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, true
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// parseSeriesStep accepts a bare number of seconds or a Go duration ("30s", "5m"). It
// returns false for a negative or malformed value; an empty string is a valid zero.
func parseSeriesStep(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

// clampFuture pins a timestamp to now when it is in the future, so a client clock that runs
// ahead cannot ask for a window that has not happened.
func clampFuture(t, now time.Time) time.Time {
	if t.After(now) {
		return now
	}
	return t
}

// seriesStep chooses the query step: the caller's request (floored to minSeriesStep), raised
// until the span needs at most maxSeriesPoints intervals, then rounded up to a whole second.
// The points-based term is ceil(span/1500): 1 h stays at 15 s, 24 h becomes 58 s and 7 d
// becomes 404 s.
func seriesStep(span, requested time.Duration) time.Duration {
	if requested < minSeriesStep {
		requested = minSeriesStep
	}
	need := time.Duration(math.Ceil(span.Seconds()/float64(maxSeriesPoints))) * time.Second
	if need < minSeriesStep {
		need = minSeriesStep
	}
	if requested < need {
		requested = need
	}
	secs := int64(math.Ceil(requested.Seconds()))
	if secs < int64(minSeriesStep/time.Second) {
		secs = int64(minSeriesStep / time.Second)
	}
	return time.Duration(secs) * time.Second
}

// alignFrom rounds t down to a whole multiple of step's seconds, using floor division so a
// pre-epoch timestamp also lands on the grid.
func alignFrom(t time.Time, step time.Duration) time.Time {
	secs := int64(step / time.Second)
	if secs <= 0 {
		secs = 1
	}
	u := t.Unix()
	rem := u % secs
	aligned := u - rem
	if rem < 0 {
		aligned -= secs
	}
	return time.Unix(aligned, 0).UTC()
}

// fitSeriesPoints aligns from down to step and widens step by one second until the aligned
// window spans at most maxSeriesPoints points. The widening only bites when the span is an
// exact multiple of the points-based step, where inclusive counting would otherwise produce
// 1501 points.
func fitSeriesPoints(from, to time.Time, step time.Duration) (time.Time, time.Duration) {
	for {
		aligned := alignFrom(from, step)
		stepSec := int64(step / time.Second)
		if stepSec <= 0 {
			stepSec = 1
		}
		if (to.Unix()-aligned.Unix())/stepSec+1 <= maxSeriesPoints {
			return aligned, step
		}
		step += time.Second
	}
}
