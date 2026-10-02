package promql

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// updateGolden rewrites the golden files instead of comparing them. It is per-package, so
// only this package's golden is regenerated: `go test ./internal/promql -update`.
var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// TestTemplateSubstitutionGolden pins every catalogue template's substituted query. One
// golden file records all of them, so adding or changing a template is a single visible
// diff; regenerate with `go test ./internal/promql -update`. The build is fixed at the
// documented example service and a 5-minute window, which exercises both placeholder
// substitutions.
func TestTemplateSubstitutionGolden(t *testing.T) {
	allowed := NewServiceSet("gateway")

	var got strings.Builder
	for _, name := range Names() {
		tmpl, err := Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", name, err)
		}
		query, err := tmpl.Build("gateway", 5*time.Minute, allowed)
		if err != nil {
			t.Fatalf("%s: Build: %v", name, err)
		}
		fmt.Fprintf(&got, "%s\t%s\n", name, query)
	}

	path := filepath.Join("testdata", "templates.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got.String()), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	if string(want) != got.String() {
		t.Errorf("templates golden mismatch; regenerate with `go test ./internal/promql -update`\n--- got ---\n%s--- want ---\n%s", got.String(), want)
	}
}
