package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// fakeLogSource is a scriptable source.LogSource for the log handler tests. It records the
// last query so the handler's defaults and validation can be asserted.
type fakeLogSource struct {
	lines  []source.LogLine
	err    error
	calls  int
	query  source.LogQuery
	tailFn func(ctx context.Context, q source.LogQuery, out chan<- source.LogLine) error
}

func (f *fakeLogSource) Query(_ context.Context, q source.LogQuery) ([]source.LogLine, error) {
	f.calls++
	f.query = q
	if f.err != nil {
		return nil, f.err
	}
	return f.lines, nil
}

func (f *fakeLogSource) Tail(ctx context.Context, q source.LogQuery, out chan<- source.LogLine) error {
	f.calls++
	f.query = q
	if f.tailFn != nil {
		return f.tailFn(ctx, q, out)
	}
	<-ctx.Done()
	return nil
}

// logsHandler wires a handler with a fixed clock and the given Loki services.
func logsHandler(f *fakeLogSource, clock *fakeClock, services ...string) *Handlers {
	return &Handlers{
		Logs: f,
		LogServices: func(context.Context) (promql.ServiceSet, error) {
			return promql.NewServiceSet(services...), nil
		},
		Now: clock.Now,
	}
}

func callLogs(h *Handlers, rawQuery string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, logsPath+"?"+rawQuery, nil)
	h.For(Route{Method: http.MethodGet, Path: logsPath}).ServeHTTP(w, r)
	return w
}

func TestLogsValidation(t *testing.T) {
	clock := newFakeClock()
	f := &fakeLogSource{}
	h := logsHandler(f, clock, "gateway")

	tests := []struct {
		name  string
		query string
	}{
		{"unknown service", "service=nope"},
		{"malformed service", "service=Gateway"},
		{"unknown level", "service=gateway&level=fatal"},
		{"bad request id", "service=gateway&request_id=has+space"},
		{"newline in contains", "service=gateway&contains=a%0Ab"},
		{"limit zero", "service=gateway&limit=0"},
		{"limit too large", "service=gateway&limit=1001"},
		{"limit not a number", "service=gateway&limit=abc"},
		{"from after to", "from=2000&to=1000"},
		{"from equals to", "from=1000&to=1000"},
		{"from unparsable", "from=soon&to=2000"},
		{"to unparsable", "from=1000&to=later"},
		{"range over seven days", "to=" + strconv.FormatInt(clock.Now().Unix(), 10) + "&from=" + strconv.FormatInt(clock.Now().Add(-8*24*time.Hour).Unix(), 10)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := callLogs(h, tt.query)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if code := seriesCode(t, w); code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
		})
	}
	if f.calls != 0 {
		t.Errorf("Query called %d times for invalid input, want 0", f.calls)
	}
}

func TestLogsDefaults(t *testing.T) {
	clock := newFakeClock()
	now := clock.Now()
	f := &fakeLogSource{}
	h := logsHandler(f, clock)

	w := callLogs(h, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := f.query.Limit; got != defaultLogLimit {
		t.Errorf("limit = %d, want %d", got, defaultLogLimit)
	}
	if got := f.query.To; !got.Equal(now) {
		t.Errorf("to = %s, want now %s", got, now)
	}
	if got := f.query.From; !got.Equal(now.Add(-time.Hour)) {
		t.Errorf("from = %s, want now-1h", got)
	}
}

func TestLogsHappyPathShape(t *testing.T) {
	clock := newFakeClock()
	at := clock.Now()
	f := &fakeLogSource{lines: []source.LogLine{
		{At: at, Service: "gateway", Level: "info", Message: "hello", Fields: map[string]any{"request_id": "abc"}},
		{At: at.Add(-time.Second), Service: "gateway", Level: "error", Message: "boom"},
	}}
	h := logsHandler(f, clock, "gateway")

	w := callLogs(h, "service=gateway&level=info&contains=hello&limit=10")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body logsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v (%s)", err, w.Body.String())
	}
	if len(body.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(body.Entries))
	}
	if body.Entries[0].Message != "hello" || body.Entries[0].Fields["request_id"] != "abc" {
		t.Errorf("entries[0] = %+v", body.Entries[0])
	}
	if body.Entries[1].Fields != nil {
		t.Errorf("entries[1].Fields = %v, want nil", body.Entries[1].Fields)
	}

	// A nil fields must be omitted, not rendered as null or {}.
	if strings.Contains(w.Body.String(), `"fields":null`) {
		t.Errorf("body renders a null fields: %s", w.Body.String())
	}
	var raw struct {
		Entries []map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw body: %v", err)
	}
	if _, ok := raw.Entries[1]["fields"]; ok {
		t.Errorf("plain line carries a fields key: %s", w.Body.String())
	}
}

func TestLogsEmptyIsAnArray(t *testing.T) {
	clock := newFakeClock()
	f := &fakeLogSource{lines: nil}
	h := logsHandler(f, clock, "gateway")

	w := callLogs(h, "service=gateway")
	if !strings.Contains(w.Body.String(), `"entries":[]`) {
		t.Errorf("body = %s, want entries:[]", w.Body.String())
	}
}

func TestLogsUnknownServiceDoesNotQuery(t *testing.T) {
	clock := newFakeClock()
	f := &fakeLogSource{}
	h := logsHandler(f, clock, "gateway")

	w := callLogs(h, "service=config")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if f.calls != 0 {
		t.Errorf("Query called %d times, want 0", f.calls)
	}
}

func TestLogsUpstreamError(t *testing.T) {
	clock := newFakeClock()
	f := &fakeLogSource{err: errors.New("loki: boom")}
	h := logsHandler(f, clock, "gateway")

	w := callLogs(h, "service=gateway")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
	if code := seriesCode(t, w); code != "upstream_error" {
		t.Errorf("code = %q, want upstream_error", code)
	}
}

func TestLogsNilSourceIsBadGateway(t *testing.T) {
	clock := newFakeClock()
	h := &Handlers{Now: clock.Now}

	w := callLogs(h, "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func itoa(n int64) string {
	return strings.TrimSpace(strings.ReplaceAll(time.Unix(n, 0).Format("2006"), ",", ""))
}
