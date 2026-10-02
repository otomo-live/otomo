package promql

import (
	"strings"
	"testing"
)

// TestOverviewGolden pins every query in the overview batch. These strings are the
// service's contract with Prometheus: a change here changes what the operator sees, so it
// should be a deliberate edit to this test too.
func TestOverviewGolden(t *testing.T) {
	want := []OverviewQuery{
		{"up", "services.up", OverviewService, `max by (service) (up{service!=""})`},
		{"rps", "services.rps", OverviewService, `sum by (service) (rate({__name__=~".+_http_requests_total",service!=""}[5m]))`},
		{"error_ratio", "services.error_ratio", OverviewService, `sum by (service) (rate({__name__=~".+_http_requests_total",service!="",status=~"5.."}[5m])) / clamp_min(sum by (service) (rate({__name__=~".+_http_requests_total",service!=""}[5m])), 1e-9)`},
		{"p95_ms", "services.p95_ms", OverviewService, `1000 * histogram_quantile(0.95, sum by (service, le) (rate({__name__=~".+_http_request_duration_seconds_bucket",service!=""}[5m])))`},
		{"version", "services.version", OverviewService, `max by (service, version) ({__name__=~".+_build_info",service!=""})`},
		{"cpu_ratio", "host.cpu", OverviewHost, `1 - avg(rate(node_cpu_seconds_total{mode="idle"}[1m]))`},
		{"mem_ratio", "host.mem", OverviewHost, `1 - sum(node_memory_MemAvailable_bytes) / sum(node_memory_MemTotal_bytes)`},
		{"disk_ratio", "host.disk", OverviewHost, `1 - sum(node_filesystem_avail_bytes{mountpoint="/"}) / sum(node_filesystem_size_bytes{mountpoint="/"})`},
		{"online_players", "online_players", OverviewHost, `sum(otomo_online_players)`},
	}

	got := Overview()
	if len(got) != len(want) {
		t.Fatalf("Overview() returned %d queries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Overview()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestOverviewHasNoPlaceholders makes sure the fixed batch never carries the template
// catalogue's {service}/{window} placeholders: these run once across every service, so a
// stray placeholder would be sent to Prometheus literally.
func TestOverviewHasNoPlaceholders(t *testing.T) {
	for _, q := range Overview() {
		if q.Name == "" || q.Card == "" {
			t.Errorf("incomplete overview query: %+v", q)
		}
		if strings.Contains(q.Query, "{service}") || strings.Contains(q.Query, "{window}") {
			t.Errorf("%s: unsubstituted placeholder in %q", q.Name, q.Query)
		}
	}
}

// TestOverviewReturnsACopy makes sure a caller cannot mutate the batch for another.
func TestOverviewReturnsACopy(t *testing.T) {
	got := Overview()
	got[0].Query = "mutated"
	if Overview()[0].Query == "mutated" {
		t.Fatal("Overview() exposed the internal slice")
	}
}
