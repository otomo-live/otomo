package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// seriesMetrics is a scriptable source.MetricsSource for the series handler. It records the
// last Range call and counts calls, so a cache test can tell one upstream query from two.
type seriesMetrics struct {
	calls int
	ts    []int64
	set   map[string][]float64
	err   error
	from  time.Time
	to    time.Time
	step  time.Duration
	query string
}

func (f *seriesMetrics) Instant(context.Context, string) (map[string]float64, error) {
	return nil, errors.New("Instant is not used by series tests")
}

func (f *seriesMetrics) Range(_ context.Context, q string, from, to time.Time, step time.Duration) ([]int64, map[string][]float64, error) {
	f.calls++
	f.query = q
	f.from, f.to, f.step = from, to, step
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.ts, f.set, nil
}

func (f *seriesMetrics) Targets(context.Context) ([]source.Target, error) {
	return nil, errors.New("Targets is not used by series tests")
}

func (f *seriesMetrics) Alerts(context.Context) ([]source.Alert, error) {
	return nil, errors.New("Alerts is not used by series tests")
}

// seriesHandler wires a handler whose allow-list is the given services and whose clock is
// fixed, so the default window and the cache TTL are deterministic.
func seriesHandler(f *seriesMetrics, clock *fakeClock, services ...string) *Handlers {
	return &Handlers{
		Metrics: f,
		AllowList: func(context.Context) (promql.ServiceSet, error) {
			return promql.NewServiceSet(services...), nil
		},
		Now: clock.Now,
	}
}

func callSeries(h *Handlers, name, rawQuery string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, seriesPath+"?"+rawQuery, nil)
	r.SetPathValue("name", name)
	h.For(Route{Method: http.MethodGet, Path: seriesPath}).ServeHTTP(w, r)
	return w
}

type wireSeries struct {
	Metric  string  `json:"metric"`
	Title   string  `json:"title"`
	Unit    string  `json:"unit"`
	Service string  `json:"service"`
	From    int64   `json:"from"`
	To      int64   `json:"to"`
	Step    int64   `json:"step"`
	Clamped bool    `json:"clamped"`
	T       []int64 `json:"t"`
	Series  []struct {
		Name   string     `json:"name"`
		Values []*float64 `json:"values"`
	} `json:"series"`
}

func decodeSeries(t *testing.T, w *httptest.ResponseRecorder) wireSeries {
	t.Helper()
	var out wireSeries
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not series JSON: %v (%s)", err, w.Body.String())
	}
	return out
}

func seriesCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the COM-5 envelope: %v (%s)", err, w.Body.String())
	}
	return body.Error.Code
}

func TestSeriesStepCalculation(t *testing.T) {
	tests := []struct {
		name      string
		span      time.Duration
		requested time.Duration
		want      time.Duration
	}{
		{"1h default", time.Hour, 0, 15 * time.Second},
		{"24h default", 24 * time.Hour, 0, 58 * time.Second},
		{"7d default", 7 * 24 * time.Hour, 0, 404 * time.Second},
		{"requested 5m on 1h", time.Hour, 5 * time.Minute, 5 * time.Minute},
		{"below the minimum", time.Hour, time.Second, 15 * time.Second},
		{"fractional rounds up", time.Hour, 20*time.Second + 500*time.Millisecond, 21 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := seriesStep(tt.span, tt.requested)
			if got != tt.want {
				t.Fatalf("seriesStep(%s, %s) = %s, want %s", tt.span, tt.requested, got, tt.want)
			}
			if points := int(tt.span/got) + 1; points > maxSeriesPoints {
				t.Fatalf("step %s yields %d points, want at most %d", got, points, maxSeriesPoints)
			}
		})
	}
}

func TestSeriesAlignmentAndPointCap(t *testing.T) {
	to := time.Unix(1735787045, 0).UTC()

	for _, from := range []time.Time{
		to.Add(-time.Hour),
		to.Add(-25 * time.Hour),
		to.Add(-8 * 24 * time.Hour),
	} {
		step := seriesStep(to.Sub(from), 0)
		aligned, step := fitSeriesPoints(from, to, step)
		if rem := aligned.Unix() % int64(step/time.Second); rem != 0 {
			t.Errorf("from = %d is not a multiple of step %s", aligned.Unix(), step)
		}
		points := (to.Unix()-aligned.Unix())/int64(step/time.Second) + 1
		if points > maxSeriesPoints {
			t.Errorf("aligned window has %d points, want at most %d", points, maxSeriesPoints)
		}
	}

	// An exact multiple of the naive step must still not exceed the cap.
	exactFrom := time.Unix(0, 0).UTC().Add(30_000 * time.Second)
	exactTo := exactFrom.Add(30_000 * time.Second)
	aligned, step := fitSeriesPoints(exactFrom, exactTo, seriesStep(exactTo.Sub(exactFrom), 0))
	if points := (exactTo.Unix()-aligned.Unix())/int64(step/time.Second) + 1; points > maxSeriesPoints {
		t.Errorf("exact-multiple window has %d points, want at most %d", points, maxSeriesPoints)
	}
}

func TestSeriesClamp30Days(t *testing.T) {
	clock := newFakeClock()
	to := clock.Now()
	f := &seriesMetrics{ts: []int64{to.Unix()}, set: map[string][]float64{"{}": {1}}}
	h := seriesHandler(f, clock, "config")

	from := to.Add(-30 * 24 * time.Hour).Unix()
	w := callSeries(h, "config", "metric=latency_p95&from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to.Unix(), 10))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	got := decodeSeries(t, w)
	if !got.Clamped {
		t.Error("clamped = false, want true for a 30-day request")
	}
	if want := int64(404); got.Step != want {
		t.Errorf("step = %d, want %d", got.Step, want)
	}
	if got.From > to.Add(-maxSeriesRange).Unix() {
		t.Errorf("from = %d, want at or before %d", got.From, to.Add(-maxSeriesRange).Unix())
	}
	if span := got.To - got.From; span < int64(maxSeriesRange/time.Second) {
		t.Errorf("span = %d, want at least %d", span, int64(maxSeriesRange/time.Second))
	}
	if got.From%got.Step != 0 {
		t.Errorf("from = %d is not aligned to step %d", got.From, got.Step)
	}
}

func TestSeriesUnknownMetric(t *testing.T) {
	clock := newFakeClock()
	f := &seriesMetrics{}
	h := seriesHandler(f, clock, "config")

	for _, query := range []string{"metric=nope", "", "metric="} {
		w := callSeries(h, "config", query)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("query %q status = %d, want 400", query, w.Code)
		}
		if code := seriesCode(t, w); code != "unknown_metric" {
			t.Errorf("query %q code = %q, want unknown_metric", query, code)
		}
	}
	if f.calls != 0 {
		t.Errorf("Range called %d times for an unknown metric, want 0", f.calls)
	}
}

func TestSeriesUnknownService(t *testing.T) {
	clock := newFakeClock()
	f := &seriesMetrics{}
	h := seriesHandler(f, clock, "config")

	for _, name := range []string{"gateway", `config"}`} {
		w := callSeries(h, name, "metric=latency_p95")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("name %q status = %d, want 400 (%s)", name, w.Code, w.Body.String())
		}
		if code := seriesCode(t, w); code != "unknown_service" {
			t.Errorf("name %q code = %q, want unknown_service", name, code)
		}
	}
	if f.calls != 0 {
		t.Errorf("Range called %d times for an unknown service, want 0", f.calls)
	}
}

func TestSeriesAllowListFailure(t *testing.T) {
	clock := newFakeClock()
	f := &seriesMetrics{}
	h := &Handlers{
		Metrics: f,
		AllowList: func(context.Context) (promql.ServiceSet, error) {
			return nil, errors.New("allow list down")
		},
		Now: clock.Now,
	}

	w := callSeries(h, "config", "metric=latency_p95")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if code := seriesCode(t, w); code != "upstream_unavailable" {
		t.Errorf("code = %q, want upstream_unavailable", code)
	}

	// A failed refresh that still carries the last good set keeps serving.
	f.set = map[string][]float64{"{}": {1}}
	h.AllowList = func(context.Context) (promql.ServiceSet, error) {
		return promql.NewServiceSet("config"), errors.New("stale")
	}
	if code := callSeries(h, "config", "metric=latency_p95").Code; code != http.StatusOK {
		t.Errorf("status with a stale set = %d, want 200", code)
	}
}

func TestSeriesValidation(t *testing.T) {
	clock := newFakeClock()
	f := &seriesMetrics{}
	h := seriesHandler(f, clock, "config")

	tests := []struct {
		name  string
		query string
	}{
		{"from equals to", "metric=latency_p95&from=1000&to=1000"},
		{"from after to", "metric=latency_p95&from=2000&to=1000"},
		{"from unparsable", "metric=latency_p95&from=soon&to=2000"},
		{"to unparsable", "metric=latency_p95&from=1000&to=later"},
		{"step unparsable", "metric=latency_p95&from=1000&to=2000&step=nope"},
		{"step negative", "metric=latency_p95&from=1000&to=2000&step=-5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := callSeries(h, "config", tt.query)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if code := seriesCode(t, w); code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
		})
	}
	if f.calls != 0 {
		t.Errorf("Range called %d times for invalid input, want 0", f.calls)
	}
}

func TestSeriesColumnarShape(t *testing.T) {
	clock := newFakeClock()
	to := clock.Now()
	from := to.Add(-time.Hour)
	f := &seriesMetrics{
		ts: []int64{from.Unix(), from.Unix() + 15, from.Unix() + 30},
		set: map[string][]float64{
			`{class="2xx"}`: {1, math.NaN(), 3},
			`{class="5xx"}`: {0, 0.5, math.NaN()},
		},
	}
	h := seriesHandler(f, clock, "config")
	w := callSeries(h, "config", "metric=rps_by_status&from="+strconv.FormatInt(from.Unix(), 10)+"&to="+strconv.FormatInt(to.Unix(), 10))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	got := decodeSeries(t, w)
	if got.Metric != "rps_by_status" || got.Service != "config" || got.Unit != "req/s" || got.Title == "" {
		t.Errorf("metadata = %+v", got)
	}
	if len(got.T) != 3 || got.T[0] != from.Unix() {
		t.Errorf("t = %v, want %d…", got.T, from.Unix())
	}
	if len(got.Series) != 2 {
		t.Fatalf("got %d series, want 2", len(got.Series))
	}
	if got.Series[0].Name != "2xx" || got.Series[1].Name != "5xx" {
		t.Fatalf("series names = %q,%q, want 2xx,5xx", got.Series[0].Name, got.Series[1].Name)
	}
	if v := got.Series[0].Values; len(v) != 3 || v[0] == nil || *v[0] != 1 || v[1] != nil || v[2] == nil || *v[2] != 3 {
		t.Errorf("2xx values = %v, want [1 null 3]", v)
	}
	if v := got.Series[1].Values; v[0] == nil || *v[0] != 0 || v[1] == nil || *v[1] != 0.5 || v[2] != nil {
		t.Errorf("5xx values = %v, want [0 0.5 null]", v)
	}
}

func TestSeriesUnlabelledNameFallback(t *testing.T) {
	clock := newFakeClock()
	f := &seriesMetrics{ts: []int64{1000}, set: map[string][]float64{"{}": {1}}}
	h := seriesHandler(f, clock, "config")

	got := decodeSeries(t, callSeries(h, "config", "metric=latency_p95&from=1000&to=2000"))
	if len(got.Series) != 1 || got.Series[0].Name != "latency_p95" {
		t.Fatalf("series = %+v, want one named latency_p95", got.Series)
	}
}

func TestSeriesUpstreamError(t *testing.T) {
	clock := newFakeClock()
	f := &seriesMetrics{err: errors.New("prometheus: boom")}
	h := seriesHandler(f, clock, "config")

	w := callSeries(h, "config", "metric=latency_p95")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if code := seriesCode(t, w); code != "upstream_error" {
		t.Errorf("code = %q, want upstream_error", code)
	}
	if msg := w.Body.String(); len(msg) == 0 {
		t.Error("empty error body")
	}
}

func TestSeriesCacheHit(t *testing.T) {
	clock := newFakeClock()
	to := clock.Now()
	f := &seriesMetrics{ts: []int64{to.Unix()}, set: map[string][]float64{"{}": {1}}}
	h := seriesHandler(f, clock, "config")
	query := "metric=latency_p95&from=" + strconv.FormatInt(to.Add(-time.Hour).Unix(), 10) + "&to=" + strconv.FormatInt(to.Unix(), 10)

	if code := callSeries(h, "config", query).Code; code != http.StatusOK {
		t.Fatalf("first status = %d", code)
	}
	if code := callSeries(h, "config", query).Code; code != http.StatusOK {
		t.Fatalf("second status = %d", code)
	}
	if f.calls != 1 {
		t.Fatalf("Range called %d times for two identical requests, want 1", f.calls)
	}

	// The TTL expires and a new upstream query runs.
	clock.Advance(11 * time.Second)
	callSeries(h, "config", query)
	if f.calls != 2 {
		t.Fatalf("Range called %d times after the TTL, want 2", f.calls)
	}
}

func TestSeriesBuildsRateWindow(t *testing.T) {
	clock := newFakeClock()
	f := &seriesMetrics{ts: []int64{clock.Now().Unix()}, set: map[string][]float64{"{}": {1}}}
	h := seriesHandler(f, clock, "config")

	to := clock.Now()
	callSeries(h, "config", "metric=latency_p95&from="+strconv.FormatInt(to.Add(-time.Hour).Unix(), 10)+"&to="+strconv.FormatInt(to.Unix(), 10))
	if f.step != 15*time.Second {
		t.Errorf("step = %s, want 15s", f.step)
	}
	if !strings.Contains(f.query, `[60s]`) || !strings.Contains(f.query, `service="config"`) {
		t.Errorf("query = %q, want a 60s window and the service label", f.query)
	}
	f.step = 0
	callSeries(h, "config", "metric=latency_p95&from="+strconv.FormatInt(to.Add(-time.Hour).Unix(), 10)+"&to="+strconv.FormatInt(to.Unix(), 10))
	if f.step != 0 {
		t.Errorf("step = %s on the cached request, want the cache to skip Range", f.step)
	}
}

func TestSeriesRFC3339AndFutureClamp(t *testing.T) {
	clock := newFakeClock()
	f := &seriesMetrics{ts: []int64{clock.Now().Unix()}, set: map[string][]float64{"{}": {1}}}
	h := seriesHandler(f, clock, "config")

	// A future `to` is pinned to the handler's clock, and an RFC3339 `from` is accepted.
	future := clock.Now().Add(time.Hour).Format(time.RFC3339)
	from := clock.Now().Add(-time.Hour).Format(time.RFC3339)
	w := callSeries(h, "config", "metric=latency_p95&from="+url.QueryEscape(from)+"&to="+url.QueryEscape(future))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	got := decodeSeries(t, w)
	if got.To != clock.Now().Unix() {
		t.Errorf("to = %d, want the clamped now %d", got.To, clock.Now().Unix())
	}
	if got.From >= got.To {
		t.Errorf("from = %d, want before to %d", got.From, got.To)
	}
}
