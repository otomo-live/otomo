package schema_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/otomo-live/otomo/services/config/internal/schema"
)

// TestCanonicalSortsAndElides covers the two normalizations a version hash depends on:
// object member order and insignificant whitespace cannot change the bytes.
func TestCanonicalSortsAndElides(t *testing.T) {
	a, err := schema.Canonical([]byte(`{ "b" : 2, "a" : 1 }`))
	if err != nil {
		t.Fatalf("Canonical(a): %v", err)
	}
	b, err := schema.Canonical([]byte(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatalf("Canonical(b): %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("canonical forms differ:\n a = %s\n b = %s", a, b)
	}
	if string(a) != `{"a":1,"b":2}` {
		t.Errorf("canonical = %s, want {\"a\":1,\"b\":2}", a)
	}
}

// TestCanonicalSortsNestedObjects checks the reordering is recursive, not just at the
// root.
func TestCanonicalSortsNestedObjects(t *testing.T) {
	got, err := schema.Canonical([]byte(`{"z":{"y":1,"x":[{"b":1,"a":2}]},"a":0}`))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	want := `{"a":0,"z":{"x":[{"a":2,"b":1}],"y":1}}`
	if string(got) != want {
		t.Errorf("canonical = %s, want %s", got, want)
	}
}

// TestCanonicalPreservesLargeIntegers is the reason this function does not use the
// RFC 8785 canonicalizer: a float64 round trip would turn 2^53+1 into 2^53.
func TestCanonicalPreservesLargeIntegers(t *testing.T) {
	got, err := schema.Canonical([]byte(`{"n":9007199254740993}`))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if string(got) != `{"n":9007199254740993}` {
		t.Errorf("canonical = %s, want the integer unchanged", got)
	}
}

// TestCanonicalIsIdempotent proves formatting an already-canonical document is a no-op,
// which is what lets a version's stored bytes be re-hashed at any time.
func TestCanonicalIsIdempotent(t *testing.T) {
	once, err := schema.Canonical([]byte(`{"b":[1,2,{"d":4,"c":3}],"a":"x"}`))
	if err != nil {
		t.Fatalf("first Canonical: %v", err)
	}
	twice, err := schema.Canonical(once)
	if err != nil {
		t.Fatalf("second Canonical: %v", err)
	}
	if !bytes.Equal(once, twice) {
		t.Errorf("Canonical is not idempotent:\n once  = %s\n twice = %s", once, twice)
	}
}

// TestCanonicalRejectsBadJSON covers the two inputs that are not legal JSON: a
// duplicate member name and invalid UTF-8. Either would give the byte representation
// two possible meanings.
func TestCanonicalRejectsBadJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"duplicate keys", `{"a":1,"a":2}`},
		{"invalid UTF-8", "{\"a\":\"\xff\"}"},
		{"truncated", `{"a":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := schema.Canonical([]byte(tt.raw)); err == nil {
				t.Fatalf("Canonical(%s) succeeded, want an error", tt.raw)
			}
		})
	}
}

// Floats are canonicalized so a value Postgres jsonb re-prints (1e2 → 100) hashes the
// same on both sides of the Config → Patch boundary; integers are not, so a large one
// survives exactly.
func TestCanonicalFloatsAreNormalisedIntegersAreNot(t *testing.T) {
	a, err := schema.Canonical([]byte(`{"x":1e2,"y":1.50,"n":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := schema.Canonical([]byte(`{"n":9007199254740993,"y":1.5,"x":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("float spellings canonicalised differently:\n%s\n%s", a, b)
	}
	if !strings.Contains(string(a), "9007199254740993") {
		t.Errorf("large integer was altered: %s", a)
	}
}
