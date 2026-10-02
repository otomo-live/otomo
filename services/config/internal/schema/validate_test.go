package schema_test

import (
	"strings"
	"testing"

	"github.com/otomo-live/otomo/services/config/internal/schema"
)

// weaponsSchema requires a weapons array whose items carry a non-negative integer
// damage. It is the schema the live validation test also PUTs, kept identical so the
// unit test and the end-to-end test assert the same pointers.
const weaponsSchema = `{
  "type": "object",
  "properties": {
    "weapons": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {"damage": {"type": "integer", "minimum": 0}},
        "required": ["damage"]
      }
    }
  },
  "required": ["weapons"]
}`

func TestValidateAcceptsAValidDocument(t *testing.T) {
	compiled, err := schema.Compile([]byte(weaponsSchema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	issues, err := schema.Validate(compiled, []byte(`{"weapons":[{"damage":3}]}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if issues != nil {
		t.Errorf("issues = %v, want nil", issues)
	}
}

// TestValidateNamesTheFailingInstance pins the pointer: a client needs the exact array
// slot, not just "somewhere in weapons".
func TestValidateNamesTheFailingInstance(t *testing.T) {
	compiled, err := schema.Compile([]byte(weaponsSchema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	issues, err := schema.Validate(compiled, []byte(`{"weapons":[{"id":"sword","damage":"lots"}]}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1", issues)
	}
	if issues[0].Pointer != "/weapons/0/damage" {
		t.Errorf("pointer = %q, want /weapons/0/damage", issues[0].Pointer)
	}
	if issues[0].Message == "" {
		t.Error("issue has no message")
	}
}

// TestValidateRequiredPointsAtTheParent proves a missing property is located where a
// client can act on it: the parent object's pointer, with the property named in the
// message.
func TestValidateRequiredPointsAtTheParent(t *testing.T) {
	compiled, err := schema.Compile([]byte(weaponsSchema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	issues, err := schema.Validate(compiled, []byte(`{}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want exactly 1", issues)
	}
	if issues[0].Pointer != "" {
		t.Errorf("pointer = %q, want the root object", issues[0].Pointer)
	}
	if !strings.Contains(issues[0].Message, "weapons") {
		t.Errorf("message %q does not name the property", issues[0].Message)
	}
}

// TestValidateCapsIssues proves a document that fails everywhere cannot produce an
// unbounded response: 150 bad items must be reported as exactly 100 issues.
func TestValidateCapsIssues(t *testing.T) {
	compiled, err := schema.Compile([]byte(`{"type":"array","items":{"type":"integer"}}`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	item := `"not an integer"`
	raw := "[" + strings.Repeat(item+",", 149) + item + "]"

	issues, err := schema.Validate(compiled, []byte(raw))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(issues) != 100 {
		t.Fatalf("len(issues) = %d, want exactly 100", len(issues))
	}
	if issues[0].Pointer != "/0" || issues[99].Pointer != "/99" {
		t.Errorf("cap did not keep library order: first %q, last %q", issues[0].Pointer, issues[99].Pointer)
	}
}

// TestNilCacheStillCompiles pins the contract the handler relies on: a nil *Cache is a
// usable validator, just not a caching one.
func TestNilCacheStillCompiles(t *testing.T) {
	var cache *schema.Cache

	compiled, err := cache.Get("gameplay", 1, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("nil Cache Get: %v", err)
	}
	if compiled == nil {
		t.Fatal("nil Cache Get returned a nil schema")
	}
}
