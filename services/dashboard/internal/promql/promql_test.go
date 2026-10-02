package promql

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNamesSortedAndComplete(t *testing.T) {
	got := Names()
	if len(got) != len(catalogue) {
		t.Fatalf("Names() returned %d names, catalogue has %d", len(got), len(catalogue))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("Names() not strictly sorted: %q then %q", got[i-1], got[i])
		}
	}
	// A copy, not the catalogue's own slice.
	got[0] = "mutated"
	if Names()[0] == "mutated" {
		t.Fatal("Names() exposed the internal slice")
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, err := Lookup("does_not_exist"); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("Lookup unknown error = %v, want ErrUnknownTemplate", err)
	}
}

func TestEveryTemplateBuilds(t *testing.T) {
	allowed := NewServiceSet("gateway")
	for _, name := range Names() {
		tmpl, err := Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", name, err)
		}
		if tmpl.Title == "" || tmpl.Unit == "" {
			t.Errorf("%s: empty Title or Unit", name)
		}
		if tmpl.Kinds == 0 {
			t.Errorf("%s: no Kinds", name)
		}
		got, err := tmpl.Build("gateway", 5*time.Minute, allowed)
		if err != nil {
			t.Fatalf("%s: Build: %v", name, err)
		}
		if strings.Contains(got, "{service}") || strings.Contains(got, "{window}") {
			t.Errorf("%s: unsubstituted placeholder in %q", name, got)
		}
		if !strings.Contains(got, "gateway") {
			t.Errorf("%s: service missing from %q", name, got)
		}
	}
}

func TestBuildWindowFloor(t *testing.T) {
	allowed := NewServiceSet("gateway")
	tmpl, err := Lookup("latency_p95")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		window time.Duration
		want   string
	}{
		{0, "60s"},
		{time.Second, "60s"},
		{30 * time.Second, "60s"},
		{59 * time.Second, "60s"},
		{time.Minute, "60s"},
		{5 * time.Minute, "300s"},
		{90*time.Second + 500*time.Millisecond, "90s"},
	}
	for _, tc := range cases {
		got, err := tmpl.Build("gateway", tc.window, allowed)
		if err != nil {
			t.Fatalf("Build(%s): %v", tc.window, err)
		}
		if !strings.Contains(got, "["+tc.want+"]") {
			t.Errorf("window %s rendered %q, want [%s]", tc.window, got, tc.want)
		}
	}
}

func TestBuildRejectsService(t *testing.T) {
	allowed := NewServiceSet("gateway")
	tmpl, _ := Lookup("rps_by_status")

	bad := []struct {
		name    string
		service string
		set     ServiceSet
	}{
		{"not in allow-list", "config", NewServiceSet("gateway")},
		{"nil allow-list", "gateway", nil},
		{"empty", "", allowed},
		{"uppercase", "Gateway", allowed},
		{"leading digit", "1gateway", allowed},
		{"quote", `gate\way`, allowed},
		{"double quote", `gate"way`, allowed},
		{"brace", "gate{way}", allowed},
		{"newline", "gate\nway", allowed},
		{"space", "gate way", allowed},
		{"too long", strings.Repeat("a", 33), NewServiceSet(strings.Repeat("a", 33))},
	}
	for _, tc := range bad {
		_, err := tmpl.Build(tc.service, time.Minute, tc.set)
		if !errors.Is(err, ErrUnknownService) {
			t.Errorf("%s: Build(%q) error = %v, want ErrUnknownService", tc.name, tc.service, err)
		}
	}
}

func TestBuildGolden(t *testing.T) {
	allowed := NewServiceSet("gateway")

	tmpl, _ := Lookup("rps_by_status")
	got, err := tmpl.Build("gateway", 5*time.Minute, allowed)
	if err != nil {
		t.Fatal(err)
	}
	want := `sum by (class) (label_replace(rate({__name__=~".+_http_requests_total",service="gateway"}[300s]), "class", "${1}xx", "status", "([0-9]).."))`
	if got != want {
		t.Errorf("rps_by_status golden mismatch:\n got %q\nwant %q", got, want)
	}

	tmpl, _ = Lookup("latency_p95")
	got, err = tmpl.Build("gateway", 5*time.Minute, allowed)
	if err != nil {
		t.Fatal(err)
	}
	want = `histogram_quantile(0.95, sum by (le) (rate({__name__=~".+_http_request_duration_seconds_bucket",service="gateway"}[300s])))`
	if got != want {
		t.Errorf("latency_p95 golden mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestServiceSetContains(t *testing.T) {
	s := NewServiceSet("a", "b")
	if !s.Contains("a") || !s.Contains("b") {
		t.Fatal("set lost a member")
	}
	if s.Contains("c") {
		t.Fatal("set contains a non-member")
	}
	if (ServiceSet(nil)).Contains("a") {
		t.Fatal("nil set contains a member")
	}
}
