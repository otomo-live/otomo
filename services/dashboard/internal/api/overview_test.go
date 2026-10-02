package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/health"
	"github.com/otomo-live/otomo/services/dashboard/internal/prometheus"
	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// queryBehavior is one fake query's scripted outcome.
type queryBehavior struct {
	result map[string]float64
	err    error
	delay  time.Duration
}

// fakeMetrics is a scriptable source.MetricsSource. It counts Instant calls, so a test can
// assert that ten concurrent overview requests ran exactly one batch.
type fakeMetrics struct {
	mu        sync.Mutex
	calls     int
	behavior  map[string]queryBehavior
	alerts    []prometheus.Alert
	alertsErr error
}

func (f *fakeMetrics) Instant(ctx context.Context, q string) (map[string]float64, error) {
	f.mu.Lock()
	f.calls++
	b := f.behavior[q]
	f.mu.Unlock()

	if b.delay > 0 {
		select {
		case <-time.After(b.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if b.err != nil {
		return nil, b.err
	}
	if b.result == nil {
		return map[string]float64{}, nil
	}
	return b.result, nil
}

func (f *fakeMetrics) Range(context.Context, string, time.Time, time.Time, time.Duration) ([]int64, map[string][]float64, error) {
	return nil, nil, errors.New("Range is not used by overview tests")
}

func (f *fakeMetrics) Targets(context.Context) ([]source.Target, error) {
	return nil, errors.New("Targets is not used by overview tests")
}

func (f *fakeMetrics) Alerts(context.Context) ([]source.Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alertsErr != nil {
		return nil, f.alertsErr
	}
	return f.alerts, nil
}

func (f *fakeMetrics) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// testingTB is the slice of testing.T and testing.B the helpers need.
type testingTB interface {
	Fatalf(format string, args ...any)
}

// queryFor returns the overview batch's query for a card name.
func queryFor(tb testingTB, name string) string {
	for _, q := range promql.Overview() {
		if q.Name == name {
			return q.Query
		}
	}
	tb.Fatalf("no overview query named %q", name)
	return ""
}

// fullBehavior scripts a plausible healthy Prometheus for the given services.
func fullBehavior(tb testingTB, services ...string) map[string]queryBehavior {
	up := map[string]float64{}
	rps := map[string]float64{}
	ratio := map[string]float64{}
	p95 := map[string]float64{}
	version := map[string]float64{}
	for _, s := range services {
		key := `{service="` + s + `"}`
		up[key] = 1
		rps[key] = 3.4
		ratio[key] = 0.01
		p95[key] = 42
		version[`{service="`+s+`",version="v1"}`] = 1
	}

	return map[string]queryBehavior{
		queryFor(tb, "up"):             {result: up},
		queryFor(tb, "rps"):            {result: rps},
		queryFor(tb, "error_ratio"):    {result: ratio},
		queryFor(tb, "p95_ms"):         {result: p95},
		queryFor(tb, "version"):        {result: version},
		queryFor(tb, "cpu_ratio"):      {result: map[string]float64{"{}": 0.18}},
		queryFor(tb, "mem_ratio"):      {result: map[string]float64{"{}": 0.44}},
		queryFor(tb, "disk_ratio"):     {result: map[string]float64{"{}": 0.61}},
		queryFor(tb, "online_players"): {result: map[string]float64{"{}": 1284}},
	}
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func overviewRoute() Route {
	return Route{Method: http.MethodGet, Path: overviewPath}
}

// callOverview runs the handler and returns the recorder.
func callOverview(h *Handlers) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, overviewPath, nil)
	h.For(overviewRoute()).ServeHTTP(w, r)
	return w
}

// wireOverview mirrors the adminui's parse shape, with pointers so a test can tell a null
// from a zero.
type wireOverview struct {
	GeneratedAt string `json:"generated_at"`
	Services    []struct {
		Name       string   `json:"name"`
		Up         bool     `json:"up"`
		Ready      bool     `json:"ready"`
		Reason     string   `json:"reason"`
		RPS        *float64 `json:"rps"`
		ErrorRatio *float64 `json:"error_ratio"`
		P95Ms      *float64 `json:"p95_ms"`
		Version    string   `json:"version"`
	} `json:"services"`
	Host struct {
		CPURatio  *float64 `json:"cpu_ratio"`
		MemRatio  *float64 `json:"mem_ratio"`
		DiskRatio *float64 `json:"disk_ratio"`
	} `json:"host"`
	OnlinePlayers *float64 `json:"online_players"`
	Alerts        []struct {
		Name     string `json:"name"`
		Severity string `json:"severity"`
		Service  string `json:"service"`
		Summary  string `json:"summary"`
		ActiveAt string `json:"active_at"`
	} `json:"alerts"`
	Degraded []string `json:"degraded"`
}

func decodeOverview(t *testing.T, w *httptest.ResponseRecorder) wireOverview {
	t.Helper()
	var out wireOverview
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not overview JSON: %v (%s)", err, w.Body.String())
	}
	return out
}

func (o wireOverview) service(t *testing.T, name string) int {
	t.Helper()
	for i, s := range o.Services {
		if s.Name == name {
			return i
		}
	}
	t.Fatalf("service %q not in %+v", name, o.Services)
	return -1
}

func TestOverviewAllFieldsMapped(t *testing.T) {
	f := &fakeMetrics{behavior: fullBehavior(t, "gateway")}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services: func() []health.Status {
			return []health.Status{{Name: "gateway", Up: true, Ready: true}}
		},
	}

	w := callOverview(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	got := decodeOverview(t, w)
	if got.GeneratedAt == "" {
		t.Error("generated_at is empty")
	}
	if _, err := time.Parse(time.RFC3339, got.GeneratedAt); err != nil {
		t.Errorf("generated_at %q is not RFC3339: %v", got.GeneratedAt, err)
	}
	if len(got.Services) != 1 {
		t.Fatalf("got %d services, want 1", len(got.Services))
	}

	s := got.Services[0]
	if s.Name != "gateway" || !s.Up || !s.Ready || s.Reason != "" {
		t.Errorf("service = %+v", s)
	}
	if s.RPS == nil || *s.RPS != 3.4 {
		t.Errorf("rps = %v, want 3.4", s.RPS)
	}
	if s.ErrorRatio == nil || *s.ErrorRatio != 0.01 {
		t.Errorf("error_ratio = %v, want 0.01", s.ErrorRatio)
	}
	if s.P95Ms == nil || *s.P95Ms != 42 {
		t.Errorf("p95_ms = %v, want 42", s.P95Ms)
	}
	if s.Version != "v1" {
		t.Errorf("version = %q, want v1", s.Version)
	}
	if got.Host.CPURatio == nil || *got.Host.CPURatio != 0.18 || got.Host.MemRatio == nil || got.Host.DiskRatio == nil {
		t.Errorf("host = %+v", got.Host)
	}
	if got.OnlinePlayers == nil || *got.OnlinePlayers != 1284 {
		t.Errorf("online_players = %v, want 1284", got.OnlinePlayers)
	}
	if len(got.Degraded) != 0 {
		t.Errorf("degraded = %v, want []", got.Degraded)
	}
	if got.Degraded == nil {
		t.Error("degraded is null, want []")
	}
}

// Alerts are ordered for the page: critical before warning before anything else, then by
// name, then by service. Pending instances are left out.
func TestOverviewAlertsSortedAndFiltered(t *testing.T) {
	f := &fakeMetrics{
		behavior: fullBehavior(t, "gateway"),
		alerts: []prometheus.Alert{
			{Name: "GameServerPoolEmpty", Severity: "warning", Summary: "pool empty", State: "firing", ActiveAt: "2024-05-01T10:03:00Z"},
			{Name: "NoFreeGameServers", Severity: "info", Summary: "pool info", State: "firing", ActiveAt: "2024-05-01T10:04:00Z"},
			{Name: "PlayerServiceDown", Severity: "critical", Service: "players", Summary: "players down", State: "firing", ActiveAt: "2024-05-01T10:02:00Z"},
			{Name: "AlertZ", Severity: "critical", Service: "a", Summary: "z is down", State: "firing", ActiveAt: "2024-05-01T10:00:00Z"},
			{Name: "PlayerServiceDown", Severity: "critical", Service: "config", Summary: "config down", State: "firing", ActiveAt: "2024-05-01T10:01:00Z"},
			{Name: "NoFreeGameServers", Severity: "critical", Summary: "still pending", State: "pending", ActiveAt: "2024-05-01T10:05:00Z"},
		},
	}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
	}

	got := decodeOverview(t, callOverview(h))
	want := []struct{ name, severity, service string }{
		{"AlertZ", "critical", "a"},
		{"PlayerServiceDown", "critical", "config"},
		{"PlayerServiceDown", "critical", "players"},
		{"GameServerPoolEmpty", "warning", ""},
		{"NoFreeGameServers", "info", ""},
	}
	if len(got.Alerts) != len(want) {
		t.Fatalf("alerts = %+v, want %d entries", got.Alerts, len(want))
	}
	for i, w := range want {
		a := got.Alerts[i]
		if a.Name != w.name || a.Severity != w.severity || a.Service != w.service {
			t.Errorf("alert[%d] = %+v, want %+v", i, a, w)
		}
	}
	if got.Alerts[0].Summary != "z is down" || got.Alerts[0].ActiveAt != "2024-05-01T10:00:00Z" {
		t.Errorf("alert[0] fields = %+v", got.Alerts[0])
	}
	if len(got.Degraded) != 0 {
		t.Errorf("degraded = %v, want none", got.Degraded)
	}
}

// An alerts failure degrades exactly the alerts card: the array is empty and other cards
// are untouched.
func TestOverviewAlertsFailureDegradesOnlyAlerts(t *testing.T) {
	f := &fakeMetrics{behavior: fullBehavior(t, "gateway"), alertsErr: errors.New("prometheus: boom")}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
	}

	got := decodeOverview(t, callOverview(h))
	if got.Alerts == nil || len(got.Alerts) != 0 {
		t.Errorf("alerts = %+v, want []", got.Alerts)
	}
	if len(got.Degraded) != 1 || got.Degraded[0] != "alerts" {
		t.Fatalf("degraded = %v, want [alerts]", got.Degraded)
	}
	s := got.Services[got.service(t, "gateway")]
	if s.RPS == nil || *s.RPS != 3.4 {
		t.Errorf("rps = %v, want 3.4 (other cards must survive)", s.RPS)
	}
	if got.Host.CPURatio == nil {
		t.Error("a failing alerts call blanked a host card")
	}
}

// A Prometheus with no alerts must still serialise "alerts": [], never null.
func TestOverviewAlertsEmptyIsArray(t *testing.T) {
	f := &fakeMetrics{behavior: fullBehavior(t, "gateway")}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
	}

	w := callOverview(h)
	got := decodeOverview(t, w)
	if got.Alerts == nil {
		t.Fatal("alerts is null, want []")
	}
	if len(got.Alerts) != 0 {
		t.Errorf("alerts = %+v, want []", got.Alerts)
	}
	if !strings.Contains(w.Body.String(), `"alerts":[]`) {
		t.Errorf("body does not render an empty alerts array: %s", w.Body.String())
	}
}

func TestOverviewOneQueryFails(t *testing.T) {
	behavior := fullBehavior(t, "gateway")
	behavior[queryFor(t, "rps")] = queryBehavior{err: errors.New("prometheus: boom")}

	f := &fakeMetrics{behavior: behavior}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
	}

	w := callOverview(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	got := decodeOverview(t, w)
	s := got.Services[got.service(t, "gateway")]
	if s.RPS != nil {
		t.Errorf("rps = %v, want null", *s.RPS)
	}
	if len(got.Degraded) != 1 || got.Degraded[0] != "services.rps" {
		t.Fatalf("degraded = %v, want [services.rps]", got.Degraded)
	}
	if s.P95Ms == nil || *s.P95Ms != 42 {
		t.Errorf("p95_ms = %v, want 42 (other cards must survive)", s.P95Ms)
	}
	if got.Host.CPURatio == nil {
		t.Error("a failing service query blanked a host card")
	}
}

// An idle service has no requests in the p95 window, so histogram_quantile answers NaN;
// a host metric can divide by zero into Inf. Both are "no data", not a failure: before
// Before the fix, json.Marshal refused them and the whole overview was a 500.
func TestOverviewNonFiniteValuesAreNull(t *testing.T) {
	behavior := fullBehavior(t, "gateway", "config")
	behavior[queryFor(t, "p95_ms")] = queryBehavior{result: map[string]float64{
		`{service="gateway"}`: math.NaN(),
		`{service="config"}`:  42,
	}}
	behavior[queryFor(t, "mem_ratio")] = queryBehavior{result: map[string]float64{"{}": math.Inf(1)}}

	f := &fakeMetrics{behavior: behavior}
	h := &Handlers{
		Metrics: f,
		AllowList: func(context.Context) (promql.ServiceSet, error) {
			return promql.NewServiceSet("gateway", "config"), nil
		},
		Services: func() []health.Status {
			return []health.Status{{Name: "gateway", Up: true, Ready: true}, {Name: "config", Up: true, Ready: true}}
		},
	}

	w := callOverview(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	got := decodeOverview(t, w)
	if p := got.Services[got.service(t, "gateway")].P95Ms; p != nil {
		t.Errorf("gateway p95_ms = %v, want null for NaN", *p)
	}
	if p := got.Services[got.service(t, "config")].P95Ms; p == nil || *p != 42 {
		t.Errorf("config p95_ms = %v, want 42", p)
	}
	if got.Host.MemRatio != nil {
		t.Errorf("mem_ratio = %v, want null for +Inf", *got.Host.MemRatio)
	}
	if got.Host.CPURatio == nil {
		t.Error("cpu_ratio blanked by another card's Inf")
	}
	if len(got.Degraded) != 0 {
		t.Errorf("degraded = %v, want none: the queries succeeded", got.Degraded)
	}
}

func TestOverviewSlowQueryDegrades(t *testing.T) {
	behavior := fullBehavior(t, "gateway")
	behavior[queryFor(t, "rps")] = queryBehavior{result: map[string]float64{`{service="gateway"}`: 9}, delay: 5 * time.Second}

	f := &fakeMetrics{behavior: behavior}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
	}

	start := time.Now()
	w := callOverview(h)
	elapsed := time.Since(start)

	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("handler took %s, want under 2.5s", elapsed)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decodeOverview(t, w)
	if len(got.Degraded) != 1 || got.Degraded[0] != "services.rps" {
		t.Fatalf("degraded = %v, want [services.rps]", got.Degraded)
	}
}

func TestOverviewConcurrentRequestsRunOneBatch(t *testing.T) {
	behavior := fullBehavior(t, "gateway")
	// A small delay keeps every request in the same flight so the singleflight group is
	// what collapses them, rather than the first batch finishing before the rest arrive.
	for q, b := range behavior {
		b.delay = 30 * time.Millisecond
		behavior[q] = b
	}

	f := &fakeMetrics{behavior: behavior}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
		Now:       newFakeClock().Now,
	}

	const callers = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			statuses[i] = callOverview(h).Code
		}(i)
	}
	close(start)
	wg.Wait()

	for i, code := range statuses {
		if code != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, code)
		}
	}
	if got, want := f.Calls(), len(promql.Overview()); got != want {
		t.Fatalf("Instant called %d times for %d concurrent requests, want %d (one batch)", got, callers, want)
	}
}

func TestOverviewSecondRequestHitsCacheThenBucketRolls(t *testing.T) {
	clock := newFakeClock()
	f := &fakeMetrics{behavior: fullBehavior(t, "gateway")}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
		Now:       clock.Now,
	}

	wantCalls := len(promql.Overview())

	if code := callOverview(h).Code; code != http.StatusOK {
		t.Fatalf("first status = %d", code)
	}
	if got := f.Calls(); got != wantCalls {
		t.Fatalf("first request ran %d queries, want %d", got, wantCalls)
	}

	// Same bucket: served from cache, no new calls.
	clock.Advance(2 * time.Second)
	callOverview(h)
	if got := f.Calls(); got != wantCalls {
		t.Fatalf("cached request ran %d queries total, want %d", got, wantCalls)
	}

	// Bucket rolls and TTL expires: one new batch.
	clock.Advance(overviewBucket)
	callOverview(h)
	if got := f.Calls(); got != 2*wantCalls {
		t.Fatalf("after bucket roll %d queries total, want %d", got, 2*wantCalls)
	}
}

func TestOverviewServicesUnionAndSort(t *testing.T) {
	behavior := fullBehavior(t, "config", "gateway")
	f := &fakeMetrics{behavior: behavior}
	h := &Handlers{
		Metrics: f,
		AllowList: func(context.Context) (promql.ServiceSet, error) {
			return promql.NewServiceSet("gateway", "config"), nil
		},
		Services: func() []health.Status {
			return []health.Status{
				{Name: "patch", Up: true, Ready: false, Reason: "boom"},
				{Name: "config", Up: true, Ready: true},
			}
		},
	}

	got := decodeOverview(t, callOverview(h))
	names := make([]string, len(got.Services))
	for i, s := range got.Services {
		names[i] = s.Name
	}
	want := []string{"config", "gateway", "patch"}
	if len(names) != len(want) {
		t.Fatalf("services = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("services = %v, want %v", names, want)
		}
	}

	config := got.Services[got.service(t, "config")]
	if !config.Up || !config.Ready {
		t.Errorf("config = %+v", config)
	}
	gateway := got.Services[got.service(t, "gateway")]
	if !gateway.Up || gateway.Ready || gateway.Reason != "not probed" {
		t.Errorf("prometheus-only gateway = %+v", gateway)
	}
	patch := got.Services[got.service(t, "patch")]
	if !patch.Up || patch.Ready || patch.Reason != "boom" {
		t.Errorf("prober-only patch = %+v", patch)
	}
}

func TestOverviewPrometheusUpFallsBackToProber(t *testing.T) {
	// The prober knows the service but Prometheus's up vector does not mention it.
	behavior := fullBehavior(t)
	behavior[queryFor(t, "up")] = queryBehavior{result: map[string]float64{}}
	f := &fakeMetrics{behavior: behavior}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet(), nil },
		Services: func() []health.Status {
			return []health.Status{{Name: "patch", Up: true, Ready: false, Reason: "boom"}}
		},
	}

	got := decodeOverview(t, callOverview(h))
	s := got.Services[got.service(t, "patch")]
	if !s.Up {
		t.Errorf("patch up = false, want the prober's true: %+v", s)
	}
}

func TestOverviewNilMetricsIsAllDegraded(t *testing.T) {
	h := &Handlers{}
	w := callOverview(h)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decodeOverview(t, w)
	if len(got.Services) != 0 {
		t.Errorf("services = %v, want []", got.Services)
	}
	if got.Degraded == nil {
		t.Fatal("degraded is null, want []")
	}
	// Every card, service and host alike, plus the alerts card.
	if len(got.Degraded) != len(promql.Overview())+1 {
		t.Fatalf("degraded = %v, want all %d cards plus alerts", got.Degraded, len(promql.Overview()))
	}
	if got.Alerts == nil {
		t.Error("alerts is null, want []")
	}
	if got.OnlinePlayers != nil || got.Host.CPURatio != nil {
		t.Errorf("host fields are not null: %+v", got.Host)
	}
}

func TestOverviewDegradedIsSortedAndDeduped(t *testing.T) {
	behavior := fullBehavior(t, "gateway")
	behavior[queryFor(t, "version")] = queryBehavior{err: errors.New("boom")}
	behavior[queryFor(t, "disk_ratio")] = queryBehavior{err: errors.New("boom")}
	behavior[queryFor(t, "online_players")] = queryBehavior{err: errors.New("boom")}

	f := &fakeMetrics{behavior: behavior}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return nil },
	}

	got := decodeOverview(t, callOverview(h))
	want := []string{"host.disk", "online_players", "services.version"}
	if len(got.Degraded) != len(want) {
		t.Fatalf("degraded = %v, want %v", got.Degraded, want)
	}
	for i := range want {
		if got.Degraded[i] != want[i] {
			t.Fatalf("degraded = %v, want %v", got.Degraded, want)
		}
	}
}

// TestOverviewHandlerOverhead is DSH-C5's 300 ms budget with an instant fake: it bounds this
// handler's own work, not Prometheus's latency.
func TestOverviewHandlerOverhead(t *testing.T) {
	f := &fakeMetrics{behavior: fullBehavior(t, "gateway")}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
		Now:       newFakeClock().Now,
	}

	start := time.Now()
	for i := 0; i < 50; i++ {
		if code := callOverview(h).Code; code != http.StatusOK {
			t.Fatalf("request %d status = %d", i, code)
		}
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("50 overview responses took %s, want well under the 300 ms budget", elapsed)
	}
}

func BenchmarkOverview(b *testing.B) {
	f := &fakeMetrics{behavior: fullBehavior(b, "gateway")}
	h := &Handlers{
		Metrics:   f,
		AllowList: func(context.Context) (promql.ServiceSet, error) { return promql.NewServiceSet("gateway"), nil },
		Services:  func() []health.Status { return []health.Status{{Name: "gateway", Up: true, Ready: true}} },
		Now:       newFakeClock().Now,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		callOverview(h)
	}
}
