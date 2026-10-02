package schema_test

import (
	"strings"
	"testing"

	"github.com/otomo-live/otomo/services/config/internal/schema"
)

func TestCompileAcceptsAValidSchema(t *testing.T) {
	compiled, err := schema.Compile([]byte(`{"type":"object","properties":{"level":{"type":"integer"}}}`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if compiled == nil {
		t.Fatal("Compile returned a nil schema")
	}
}

func TestCompileRejectsBadDocuments(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"not an object: array", `[]`},
		{"not an object: string", `"gameplay"`},
		{"not an object: number", `12`},
		{"not an object: boolean", `true`},
		{"not an object: null", `null`},
		{"duplicate keys", `{"type":"object","type":"string"}`},
		{"invalid UTF-8", "{\"type\":\"\xff\"}"},
		{"invalid schema", `{"type":12}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := schema.Compile([]byte(tt.raw)); err == nil {
				t.Fatalf("Compile(%s) succeeded, want an error", tt.raw)
			}
		})
	}
}

// TestCompileRefusesExternalReferences is the security property: the library's default
// loader reads files, so a file: $ref must fail without reading the file. The "root:"
// assertion is what distinguishes "refused before reading" from "read and parsed a
// schema that happened to be invalid".
func TestCompileRefusesExternalReferences(t *testing.T) {
	refs := []string{
		`{"$ref":"file:///etc/passwd"}`,
		`{"$ref":"http://example.com/s.json"}`,
	}

	for _, raw := range refs {
		t.Run(raw, func(t *testing.T) {
			_, err := schema.Compile([]byte(raw))
			if err == nil {
				t.Fatalf("Compile(%s) succeeded, want an error", raw)
			}
			if strings.Contains(err.Error(), "root:") {
				t.Fatalf("error leaked file contents: %v", err)
			}
		})
	}
}

// TestCompileAssertsFormat proves AssertFormat is on: without it draft 2020-12 treats
// format as an annotation and would accept the invalid address.
func TestCompileAssertsFormat(t *testing.T) {
	compiled, err := schema.Compile([]byte(`{"type":"string","format":"email"}`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if err := compiled.Validate("not-an-email"); err == nil {
		t.Error("Validate accepted a non-email with AssertFormat on")
	}
	if err := compiled.Validate("someone@example.com"); err != nil {
		t.Errorf("Validate rejected a valid email: %v", err)
	}
}

// TestCacheReturnsTheSameCompiledSchema pins the cache's purpose: a second Get for an
// immutable version must not compile again, which pointer equality demonstrates.
func TestCacheReturnsTheSameCompiledSchema(t *testing.T) {
	var cache schema.Cache
	raw := []byte(`{"type":"object"}`)

	first, err := cache.Get("gameplay", 1, raw)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	second, err := cache.Get("gameplay", 1, raw)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if first != second {
		t.Error("the second Get returned a different pointer; the schema was recompiled")
	}

	// A different version is a different entry.
	other, err := cache.Get("gameplay", 2, []byte(`{"type":"string"}`))
	if err != nil {
		t.Fatalf("Get for another version: %v", err)
	}
	if other == first {
		t.Error("two versions share a compiled schema")
	}
}

// TestCacheSurfacesCompileErrors proves a miss with a bad document does not poison the
// cache or panic.
func TestCacheSurfacesCompileErrors(t *testing.T) {
	var cache schema.Cache
	if _, err := cache.Get("gameplay", 1, []byte(`{"type":12}`)); err == nil {
		t.Fatal("Get accepted an invalid schema")
	}

	compiled, err := cache.Get("gameplay", 1, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("Get after a failed compile: %v", err)
	}
	if compiled == nil {
		t.Fatal("Get returned a nil schema")
	}
}
