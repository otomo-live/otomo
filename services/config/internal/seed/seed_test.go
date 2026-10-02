package seed_test

import (
	"encoding/json"
	"path"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/otomo-live/otomo/services/config/internal/seed"
)

// These tests need no database: they cover Load, which is pure parsing and checking.

// TestLoadEmbeddedFolder is the check that the tree the binary carries is itself valid.
// It asserts the exact set of names, because a namespace silently missing from the
// embed would be a feature missing from every deployment.
func TestLoadEmbeddedFolder(t *testing.T) {
	namespaces, err := seed.Load(seed.FS)
	if err != nil {
		t.Fatalf("Load(embedded): %v", err)
	}

	// Name → audience. session.rules is the one server-only namespace: it must never
	// reach a client manifest.
	want := map[string]string{
		"balance.player":  "client",
		"features.flags":  "client",
		"session.rules":   "server",
		"ui.motd":         "client",
		"ui.presentation": "client",
	}
	if len(namespaces) != len(want) {
		t.Fatalf("Load returned %d namespaces %v, want %d", len(namespaces), namespaceNames(namespaces), len(want))
	}
	for _, ns := range namespaces {
		audience, ok := want[ns.Name]
		if !ok {
			t.Errorf("unexpected namespace %q", ns.Name)
		}
		if ns.Audience != audience {
			t.Errorf("%s: audience = %q, want %q", ns.Name, ns.Audience, audience)
		}
		if ns.Description == "" {
			t.Errorf("%s: description is empty", ns.Name)
		}
		if len(ns.Schema) == 0 || len(ns.Draft) == 0 {
			t.Errorf("%s: schema or draft is empty", ns.Name)
		}
	}
}

// TestLoadFailures checks that each way a folder can be wrong produces an error that
// names the offending file, because the operator has three files per namespace and no
// other way to tell which one to fix.
func TestLoadFailures(t *testing.T) {
	const validSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","title":"T","description":"d","type":"object","additionalProperties":false,"properties":{"count":{"type":"integer","description":"d","minimum":0}}}`
	const validDraft = `{"count":1}`
	const validMeta = `{"audience":"client","description":"test"}`

	tests := []struct {
		name     string
		dir      string
		schema   string
		draft    string
		meta     string
		wantFile string
	}{
		{
			name:     "bad namespace name",
			dir:      "Bad.Name",
			schema:   validSchema,
			draft:    validDraft,
			meta:     validMeta,
			wantFile: "namespace.json",
		},
		{
			name:     "bad audience",
			dir:      "bad.audience",
			schema:   validSchema,
			draft:    validDraft,
			meta:     `{"audience":"everyone","description":"test"}`,
			wantFile: "namespace.json",
		},
		{
			name:     "uncompilable schema",
			dir:      "bad.schema",
			schema:   `{"$schema":"https://json-schema.org/draft/2020-12/schema","$ref":"http://example.com/not-allowed"}`,
			draft:    validDraft,
			meta:     validMeta,
			wantFile: "schema.json",
		},
		{
			name:     "invalid draft",
			dir:      "bad.draft",
			schema:   validSchema,
			draft:    `{"count":"not an integer"}`,
			meta:     validMeta,
			wantFile: "draft.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{
				path.Join(tt.dir, "namespace.json"): &fstest.MapFile{Data: []byte(tt.meta)},
				path.Join(tt.dir, "schema.json"):    &fstest.MapFile{Data: []byte(tt.schema)},
				path.Join(tt.dir, "draft.json"):     &fstest.MapFile{Data: []byte(tt.draft)},
			}

			_, err := seed.Load(fsys)
			if err == nil {
				t.Fatal("Load accepted an invalid folder")
			}
			if !strings.Contains(err.Error(), tt.wantFile) {
				t.Errorf("error %q does not name %s", err, tt.wantFile)
			}
		})
	}
}

// TestLoadMissingFile names the file even when it is not there to read.
func TestLoadMissingFile(t *testing.T) {
	fsys := fstest.MapFS{
		path.Join("lonely", "namespace.json"): &fstest.MapFile{Data: []byte(`{"audience":"client"}`)},
	}
	_, err := seed.Load(fsys)
	if err == nil || !strings.Contains(err.Error(), "schema.json") {
		t.Fatalf("Load error = %v, want one naming schema.json", err)
	}
}

func namespaceNames(namespaces []seed.Namespace) []string {
	names := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		names = append(names, ns.Name)
	}
	return names
}

func equalJSON(t *testing.T, got, want []byte) {
	t.Helper()

	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("got is not JSON: %v", err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}
