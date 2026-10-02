package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	clientprom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/otomo-live/otomo/services/dashboard/internal/api"
	"github.com/otomo-live/otomo/services/dashboard/internal/auditsrc"
	"github.com/otomo-live/otomo/services/dashboard/internal/cache"
	"github.com/otomo-live/otomo/services/dashboard/internal/loki"
	"github.com/otomo-live/otomo/services/dashboard/internal/prometheus"
	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// The server is the single implementation of every instrumentation seam. These assertions
// are compile-time: a seam that stops being satisfied fails the build before any test runs.
var (
	_ prometheus.Observer = (*Server)(nil)
	_ loki.Observer       = (*Server)(nil)
	_ auditsrc.Observer   = (*Server)(nil)
	_ cache.Observer      = (*Server)(nil)
	_ api.Observer        = (*Server)(nil)
)

// renderMetrics serialises a registry exactly as the internal /metrics endpoint does.
func renderMetrics(t *testing.T, reg *clientprom.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// TestMetricsPreInitialised pins the closed label sets: every upstream/result, endpoint/
// result, card and the tail gauge exists at 0 before the first event, so an alert that
// references one of them does not flap while it waits for the first sample.
func TestMetricsPreInitialised(t *testing.T) {
	m := newMetrics("test")
	body := renderMetrics(t, m.registry)

	for _, want := range []string{
		`dashboard_upstream_requests_total{result="ok",upstream="prometheus"} 0`,
		`dashboard_upstream_requests_total{result="error",upstream="loki"} 0`,
		`dashboard_upstream_requests_total{result="timeout",upstream="config_audit"} 0`,
		`dashboard_upstream_requests_total{result="timeout",upstream="admin_auth_audit"} 0`,
		`dashboard_upstream_duration_seconds_count{upstream="prometheus"} 0`,
		`dashboard_upstream_duration_seconds_count{upstream="admin_auth_audit"} 0`,
		`dashboard_cache_requests_total{endpoint="overview",result="hit"} 0`,
		`dashboard_cache_requests_total{endpoint="overview",result="miss"} 0`,
		`dashboard_cache_requests_total{endpoint="overview",result="shared"} 0`,
		`dashboard_cache_requests_total{endpoint="series",result="hit"} 0`,
		`dashboard_overview_degraded_total{card="services.rps"} 0`,
		`dashboard_overview_degraded_total{card="online_players"} 0`,
		`dashboard_tail_streams 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not contain pre-initialised series %q", want)
		}
	}
}

// TestInstrumentMethodsRecord ties each seam method to its exported series name and labels.
func TestInstrumentMethodsRecord(t *testing.T) {
	m := newMetrics("test")
	srv := &Server{metrics: m}

	srv.UpstreamRequest("prometheus", "ok", 25*time.Millisecond)
	srv.UpstreamRequest("prometheus", "timeout", time.Second)
	srv.CacheRequest("overview", "miss")
	srv.CacheRequest("series", "hit")
	srv.OverviewDegraded("services.rps")
	srv.TailStreams(1)

	body := renderMetrics(t, m.registry)
	for _, want := range []string{
		`dashboard_upstream_requests_total{result="ok",upstream="prometheus"} 1`,
		`dashboard_upstream_requests_total{result="timeout",upstream="prometheus"} 1`,
		`dashboard_upstream_duration_seconds_count{upstream="prometheus"} 2`,
		`dashboard_cache_requests_total{endpoint="overview",result="miss"} 1`,
		`dashboard_cache_requests_total{endpoint="series",result="hit"} 1`,
		`dashboard_overview_degraded_total{card="services.rps"} 1`,
		`dashboard_tail_streams 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not contain %q", want)
		}
	}
}

// overviewDegradeSource is a MetricsSource that fails one named overview query and answers
// everything else with an empty vector, which is enough to drive the handler.
type overviewDegradeSource struct {
	fail string
}

func (s *overviewDegradeSource) Instant(_ context.Context, q string) (map[string]float64, error) {
	if q == s.fail {
		return nil, errors.New("prometheus: boom")
	}
	return map[string]float64{}, nil
}

func (s *overviewDegradeSource) Range(context.Context, string, time.Time, time.Time, time.Duration) ([]int64, map[string][]float64, error) {
	return nil, nil, nil
}

func (s *overviewDegradeSource) Targets(context.Context) ([]source.Target, error) { return nil, nil }

func (s *overviewDegradeSource) Alerts(context.Context) ([]source.Alert, error) { return nil, nil }

// overviewQueryFor returns a named overview query, so the test fails one exact card.
func overviewQueryFor(t *testing.T, name string) string {
	t.Helper()
	for _, q := range promql.Overview() {
		if q.Name == name {
			return q.Query
		}
	}
	t.Fatalf("no overview query named %q", name)
	return ""
}

// TestOverviewDegradedMetric drives a real overview through the server with one failing
// query and reads dashboard_overview_degraded_total{card=...} back off /metrics.
func TestOverviewDegradedMetric(t *testing.T) {
	handlers := &api.Handlers{Metrics: &overviewDegradeSource{fail: overviewQueryFor(t, "rps")}}
	h := newHarnessWithHandlers(t, nil, handlers)
	handlers.Observer = h.srv

	resp := h.do(t, h.public, http.MethodGet, "/api/admin/dashboard/overview", h.token(t, []string{"viewer"}, nil))
	if resp.status != http.StatusOK {
		t.Fatalf("overview status = %d, want 200 (%s)", resp.status, resp.body)
	}
	if got := counterValue(t, h, "dashboard_overview_degraded_total", `card="services.rps"`); got != 1 {
		t.Errorf("dashboard_overview_degraded_total{card=services.rps} = %g, want 1", got)
	}
}

// gaugeValue reads an unlabelled gauge off the internal /metrics endpoint, returning 0 when
// the sample is absent.
func gaugeValue(t *testing.T, h *harness, name string) float64 {
	t.Helper()
	body := h.do(t, h.internal, http.MethodGet, "/metrics", "").body
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+" ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("cannot read a value out of %q", line)
		}
		var v float64
		if _, err := fmt.Sscanf(fields[1], "%g", &v); err != nil {
			t.Fatalf("cannot parse %q: %v", fields[1], err)
		}
		return v
	}
	return 0
}

// waitGauge polls an unlabelled gauge to want, with a bounded deadline.
func waitGauge(t *testing.T, h *harness, name string, want float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := gaugeValue(t, h, name); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached %g (last %g)", name, want, gaugeValue(t, h, name))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTailStreamsGauge opens a real SSE tail and asserts dashboard_tail_streams moves to 1
// while it holds a slot and back to 0 once the client disconnects.
func TestTailStreamsGauge(t *testing.T) {
	src := &tailSource{lines: make(chan source.LogLine)}
	handlers := tailHandlers(src, time.Hour)
	h := newHarnessWithHandlers(t, nil, handlers)
	handlers.Observer = h.srv

	resp, cancel := openTail(t, h, h.token(t, []string{"viewer"}, nil))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("tail status = %d, want 200", resp.StatusCode)
	}
	defer resp.Body.Close()

	waitGauge(t, h, "dashboard_tail_streams", 1)

	cancel()
	waitGauge(t, h, "dashboard_tail_streams", 0)
}
