package prometheus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixtureServer serves one fixture file at every path and counts requests.
func fixtureServer(t *testing.T, name string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

func TestRangeColumnar(t *testing.T) {
	srv, _ := fixtureServer(t, "range.json")
	c := New(srv.URL, srv.Client(), time.Second)

	from := time.Unix(1789000000, 0)
	to := time.Unix(1789000060, 0)
	ts, series, err := c.Range(context.Background(), "q", from, to, 15*time.Second)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}

	wantTS := []int64{1789000000, 1789000015, 1789000030, 1789000045, 1789000060}
	if len(ts) != len(wantTS) {
		t.Fatalf("ts = %v, want %v", ts, wantTS)
	}
	for i := range wantTS {
		if ts[i] != wantTS[i] {
			t.Fatalf("ts[%d] = %d, want %d", i, ts[i], wantTS[i])
		}
	}

	if len(series) != 2 {
		t.Fatalf("got %d series, want 2: %v", len(series), series)
	}

	key1 := `{method="GET",route="/health",service="gateway",status="200"}`
	key2 := `{method="GET",route="/players",service="gateway",status="500"}`
	want1 := []float64{10, 20, 30, 40, 50}
	want2 := []float64{1, math.NaN(), 3, math.NaN(), 5}

	assertSeries(t, series, key1, want1)
	assertSeries(t, series, key2, want2)
}

// assertSeries checks one series equals want, treating NaN as equal to NaN.
func assertSeries(t *testing.T, series map[string][]float64, key string, want []float64) {
	t.Helper()
	got, ok := series[key]
	if !ok {
		t.Fatalf("missing series %s; have %v", key, keysOf(series))
	}
	if len(got) != len(want) {
		t.Fatalf("%s length = %d, want %d: %v", key, len(got), len(want), got)
	}
	for i := range want {
		if math.IsNaN(want[i]) {
			if !math.IsNaN(got[i]) {
				t.Errorf("%s[%d] = %v, want NaN", key, i, got[i])
			}
			continue
		}
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %v, want %v", key, i, got[i], want[i])
		}
	}
}

func keysOf(m map[string][]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestInstantVectorAndScalar(t *testing.T) {
	ctx := context.Background()

	srv, _ := fixtureServer(t, "vector.json")
	c := New(srv.URL, srv.Client(), time.Second)
	got, err := c.Instant(ctx, "q")
	if err != nil {
		t.Fatalf("Instant vector: %v", err)
	}
	want := map[string]float64{
		`{instance="gateway:9090",job="gateway",service="gateway"}`: 1,
		`{service="config"}`: 0,
	}
	if len(got) != len(want) {
		t.Fatalf("vector = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("vector[%s] = %v, want %v", k, got[k], v)
		}
	}

	srv2, _ := fixtureServer(t, "scalar.json")
	c2 := New(srv2.URL, srv2.Client(), time.Second)
	got2, err := c2.Instant(ctx, "q")
	if err != nil {
		t.Fatalf("Instant scalar: %v", err)
	}
	if len(got2) != 1 || got2["{}"] != 42.5 {
		t.Fatalf("scalar = %v, want {\"{}\": 42.5}", got2)
	}
}

func TestInstantEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client(), time.Second)
	got, err := c.Instant(context.Background(), "q")
	if err != nil {
		t.Fatalf("Instant: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("empty result = %v, want empty non-nil map", got)
	}
}

func TestErrorMapping(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "error.json"))
	if err != nil {
		t.Fatal(err)
	}

	for _, code := range []int{http.StatusOK, http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("HTTP%d", code), func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(code)
				_, _ = w.Write(body)
			}))
			t.Cleanup(srv.Close)

			c := New(srv.URL, srv.Client(), time.Second)
			_, err := c.Instant(context.Background(), "q")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "bad_data") {
				t.Errorf("error %q does not carry errorType", err)
			}
			if !strings.Contains(err.Error(), "parse error") {
				t.Errorf("error %q does not carry message", err)
			}
			if strings.Contains(err.Error(), srv.URL) {
				t.Errorf("error %q leaks the URL", err)
			}
			if gotPath != "/api/v1/query" {
				t.Errorf("path = %q, want /api/v1/query", gotPath)
			}
		})
	}
}

func TestInstantTransportErrorHasNoURL(t *testing.T) {
	c := New("http://127.0.0.1:1", nil, time.Second)
	_, err := c.Instant(context.Background(), "q")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "http://") {
		t.Fatalf("transport error leaks URL: %q", err)
	}
}

func TestBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, maxBodyBytes+1))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client(), 5*time.Second)
	_, err := c.Instant(context.Background(), "q")
	if err == nil {
		t.Fatal("expected oversized body error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %q, want body-cap error", err)
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	c := New(srv.URL, srv.Client(), 50*time.Millisecond)
	start := time.Now()
	_, err := c.Instant(context.Background(), "q")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Instant took %s, timeout did not bound the call", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestTargets(t *testing.T) {
	srv, count := fixtureServer(t, "targets.json")
	c := New(srv.URL, srv.Client(), time.Second)

	targets, err := c.Targets(context.Background())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if count.Load() != 1 {
		t.Fatalf("Targets made %d requests, want 1", count.Load())
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2 (target without service skipped): %+v", len(targets), targets)
	}

	if targets[0].Service != "gateway" || !targets[0].Up || targets[0].ScrapeError != "" {
		t.Errorf("gateway target = %+v", targets[0])
	}
	if want := time.Date(2024, 5, 1, 10, 0, 0, 123456789, time.UTC); !targets[0].LastScrape.Equal(want) {
		t.Errorf("gateway LastScrape = %s, want %s", targets[0].LastScrape, want)
	}
	if targets[1].Service != "config" || targets[1].Up || targets[1].ScrapeError != "connection refused" {
		t.Errorf("config target = %+v", targets[1])
	}
}

func TestAlerts(t *testing.T) {
	srv, count := fixtureServer(t, "alerts.json")
	c := New(srv.URL, srv.Client(), time.Second)

	alerts, err := c.Alerts(context.Background())
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if count.Load() != 1 {
		t.Fatalf("Alerts made %d requests, want 1", count.Load())
	}
	if len(alerts) != 3 {
		t.Fatalf("got %d alerts, want 3: %+v", len(alerts), alerts)
	}

	firing := alerts[0]
	if firing.Name != "PlayerServiceDown" || firing.Severity != "critical" || firing.Service != "players" ||
		firing.Summary != "Player service is down" || firing.State != "firing" ||
		firing.ActiveAt != "2024-05-01T10:00:00.123456789Z" {
		t.Errorf("critical firing alert = %+v", firing)
	}

	warning := alerts[1]
	if warning.Name != "GameServerPoolEmpty" || warning.Severity != "warning" || warning.Service != "" ||
		warning.Summary != "Game server pool is empty" || warning.State != "firing" {
		t.Errorf("warning firing alert = %+v", warning)
	}

	pending := alerts[2]
	if pending.Name != "NoFreeGameServers" || pending.Severity != "critical" ||
		pending.Service != "lobby" || pending.State != "pending" {
		t.Errorf("pending alert = %+v", pending)
	}
}

// An alerts API error envelope becomes an error carrying Prometheus's own errorType and
// message, never the request URL, exactly like the other calls.
func TestAlertsErrorEnvelope(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "error.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/v1/alerts" {
			t.Errorf("path = %q, want /api/v1/alerts", got)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client(), time.Second)
	_, err = c.Alerts(context.Background())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "bad_data") || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("error = %q, want the upstream errorType and message", err)
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("error %q leaks the URL", err)
	}
}

func TestAllowListCachesWithinTTL(t *testing.T) {
	srv, count := fixtureServer(t, "targets.json")
	c := New(srv.URL, srv.Client(), time.Second)

	for i := 0; i < 3; i++ {
		set, err := c.AllowList(context.Background())
		if err != nil {
			t.Fatalf("AllowList #%d: %v", i, err)
		}
		if !set.Contains("gateway") || !set.Contains("config") {
			t.Fatalf("AllowList #%d = %v", i, set)
		}
	}
	if count.Load() != 1 {
		t.Fatalf("AllowList made %d requests within TTL, want 1", count.Load())
	}
}

func TestAllowListRefreshesAfterTTL(t *testing.T) {
	srv, count := fixtureServer(t, "targets.json")
	c := New(srv.URL, srv.Client(), time.Second)
	c.allowTTL = 20 * time.Millisecond

	if _, err := c.AllowList(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := c.AllowList(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatalf("AllowList made %d requests, want 2 after TTL", count.Load())
	}
}

func TestAllowListConcurrentSingleflight(t *testing.T) {
	var count atomic.Int64
	body, err := os.ReadFile(filepath.Join("testdata", "targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client(), time.Second)

	const callers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			set, err := c.AllowList(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if !set.Contains("gateway") {
				errs <- fmt.Errorf("set missing gateway: %v", set)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatalf("concurrent AllowList made %d requests, want 1", count.Load())
	}
}

func TestAllowListErrorKeepsLastGoodSet(t *testing.T) {
	var fail atomic.Bool
	body, err := os.ReadFile(filepath.Join("testdata", "targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"status":"error","errorType":"internal","error":"boom"}`)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client(), time.Second)
	c.allowTTL = 20 * time.Millisecond

	if _, err := c.AllowList(context.Background()); err != nil {
		t.Fatal(err)
	}

	fail.Store(true)
	time.Sleep(60 * time.Millisecond)
	set, err := c.AllowList(context.Background())
	if err == nil {
		t.Fatal("expected refresh error, got nil")
	}
	if !set.Contains("gateway") {
		t.Fatalf("error refresh dropped the last good set: %v", set)
	}
}

func TestSeriesKey(t *testing.T) {
	cases := []struct {
		name   string
		metric map[string]string
		want   string
	}{
		{"empty", nil, "{}"},
		{"only name", map[string]string{"__name__": "up"}, "{}"},
		{"drops name and sorts", map[string]string{
			"status": "200", "route": "/a", "__name__": "svc_http_requests_total", "service": "gateway",
		}, `{route="/a",service="gateway",status="200"}`},
		{"single", map[string]string{"service": "config"}, `{service="config"}`},
	}
	for _, tc := range cases {
		if got := seriesKey(tc.metric); got != tc.want {
			t.Errorf("%s: seriesKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLabelValue(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		label string
		want  string
	}{
		{"service", `{service="gateway"}`, "service", "gateway"},
		{"second label", `{route="/a",service="config",status="200"}`, "service", "config"},
		{"version", `{service="dashboard",version="4be751b"}`, "version", "4be751b"},
		{"missing", `{service="gateway"}`, "version", ""},
		{"empty key", "{}", "service", ""},
		{"no braces", "service=gateway", "service", ""},
		{"malformed", `{service=gateway}`, "service", ""},
		{"trailing comma", `{service="gateway",}`, "service", "gateway"},
	}
	for _, tc := range cases {
		if got := LabelValue(tc.key, tc.label); got != tc.want {
			t.Errorf("%s: LabelValue(%q, %q) = %q, want %q", tc.name, tc.key, tc.label, got, tc.want)
		}
	}
}

func TestParseValueSpecial(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
	}{
		{`"1.5"`, 1.5},
		{`"NaN"`, math.NaN()},
		{`"+Inf"`, math.Inf(1)},
		{`"-Inf"`, math.Inf(-1)},
		{`2.5`, 2.5},
	}
	for _, tc := range cases {
		got, err := parseValue([]byte(tc.raw))
		if err != nil {
			t.Fatalf("parseValue(%s): %v", tc.raw, err)
		}
		if math.IsNaN(tc.want) {
			if !math.IsNaN(got) {
				t.Errorf("parseValue(%s) = %v, want NaN", tc.raw, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("parseValue(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestRangeRequestParams(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client(), time.Second)
	from := time.Unix(1789000000, 0)
	to := time.Unix(1789000060, 0)
	if _, _, err := c.Range(context.Background(), `up{service="gateway"}`, from, to, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := gotQuery.Get("query"); got != `up{service="gateway"}` {
		t.Errorf("query = %q", got)
	}
	if got := gotQuery.Get("start"); got != "1789000000" {
		t.Errorf("start = %q", got)
	}
	if got := gotQuery.Get("end"); got != "1789000060" {
		t.Errorf("end = %q", got)
	}
	if got := gotQuery.Get("step"); got != "15" {
		t.Errorf("step = %q", got)
	}
}

func TestNewDefaults(t *testing.T) {
	c := New("http://example/", nil, 0)
	if c.baseURL != "http://example" {
		t.Errorf("baseURL = %q, want trailing slash trimmed", c.baseURL)
	}
	if c.timeout != defaultTimeout {
		t.Errorf("timeout = %s, want %s", c.timeout, defaultTimeout)
	}
	if c.allowTTL != allowListTTL {
		t.Errorf("allowTTL = %s, want %s", c.allowTTL, allowListTTL)
	}
}

// A failed refresh is retried after allowRetry, not after the full TTL, so a
// Prometheus blip does not make every service unknown for half a minute.
func TestAllowListRetriesSoonAfterError(t *testing.T) {
	var fail atomic.Bool
	var calls atomic.Int64
	body, err := os.ReadFile(filepath.Join("testdata", "targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client(), time.Second)
	c.allowTTL = time.Hour
	c.allowRetry = 20 * time.Millisecond

	fail.Store(true)
	if _, err := c.AllowList(context.Background()); err == nil {
		t.Fatal("expected an error while Prometheus is down")
	}
	fail.Store(false)
	time.Sleep(60 * time.Millisecond)
	set, err := c.AllowList(context.Background())
	if err != nil || !set.Contains("gateway") {
		t.Fatalf("after recovery: set=%v err=%v, want gateway and no error", set, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

// The refresh is shared by every waiting caller, so the context of the request that
// started it being cancelled must not fail it.
func TestAllowListRefreshSurvivesStarterCancel(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client(), 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.AllowList(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("AllowList did not return")
	}
	set, err := c.AllowList(context.Background())
	if err != nil || !set.Contains("gateway") {
		t.Fatalf("cached set after a cancelled starter: set=%v err=%v", set, err)
	}
}

// upstreamEvent is one recorded instrumentation callback.
type upstreamEvent struct {
	upstream string
	result   string
	d        time.Duration
}

// recordingObserver captures the seam's callbacks so a test can assert the exact upstream
// label and result classification.
type recordingObserver struct {
	mu     sync.Mutex
	events []upstreamEvent
}

func (o *recordingObserver) UpstreamRequest(upstream, result string, d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, upstreamEvent{upstream: upstream, result: result, d: d})
}

func (o *recordingObserver) last(t *testing.T) upstreamEvent {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.events) == 0 {
		t.Fatal("no upstream events recorded")
	}
	return o.events[len(o.events)-1]
}

// TestObserverRecordsUpstreamResult pins the instrumentation seam: every request reports
// upstream="prometheus" with ok, error or timeout, and a timeout is never miscounted as a
// generic error.
func TestObserverRecordsUpstreamResult(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv, _ := fixtureServer(t, "vector.json")
		c := New(srv.URL, srv.Client(), time.Second)
		obs := &recordingObserver{}
		c.Observer = obs

		if _, err := c.Instant(context.Background(), "q"); err != nil {
			t.Fatalf("Instant: %v", err)
		}
		if e := obs.last(t); e.upstream != "prometheus" || e.result != "ok" {
			t.Fatalf("event = %+v, want prometheus/ok", e)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(func() {
			close(release)
			srv.Close()
		})

		c := New(srv.URL, srv.Client(), 30*time.Millisecond)
		obs := &recordingObserver{}
		c.Observer = obs

		if _, err := c.Instant(context.Background(), "q"); err == nil {
			t.Fatal("expected a timeout error")
		}
		if e := obs.last(t); e.upstream != "prometheus" || e.result != "timeout" {
			t.Fatalf("event = %+v, want prometheus/timeout", e)
		}
	})

	t.Run("error", func(t *testing.T) {
		srv, _ := fixtureServer(t, "error.json")
		c := New(srv.URL, srv.Client(), time.Second)
		obs := &recordingObserver{}
		c.Observer = obs

		if _, err := c.Instant(context.Background(), "q"); err == nil {
			t.Fatal("expected an upstream error")
		}
		if e := obs.last(t); e.upstream != "prometheus" || e.result != "error" {
			t.Fatalf("event = %+v, want prometheus/error", e)
		}
	})
}
