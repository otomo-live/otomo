package seed_test

import (
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/seed"
)

// TestSessionRulesSchema pins the contract Session's rules loader (LB-1) reads: the
// seeded draft is valid, and the schema refuses the mistakes a designer could make in
// the admin UI. Cross-field rules (max_length >= min_length, default among allowed)
// are Session's to enforce; JSON Schema cannot express them.
func TestSessionRulesSchema(t *testing.T) {
	rawSchema, err := fs.ReadFile(seed.FS, "session.rules/schema.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	compiled, err := schema.Compile(rawSchema)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	draft, err := fs.ReadFile(seed.FS, "session.rules/draft.json")
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}

	if issues, err := schema.Validate(compiled, draft); err != nil || len(issues) != 0 {
		t.Fatalf("seeded draft: issues %v, err %v", issues, err)
	}

	cases := []struct {
		name   string
		mutate func(doc map[string]any)
	}{
		{"unknown top-level key", func(d map[string]any) { d["matchmaking"] = map[string]any{} }},
		{"missing lobby", func(d map[string]any) { delete(d, "lobby") }},
		{"party size zero", func(d map[string]any) { obj(d, "party")["max_size"] = 0 }},
		{"party size fractional", func(d map[string]any) { obj(d, "party")["max_size"] = 2.5 }},
		{"negative rename cooldown", func(d map[string]any) { obj(d, "names")["rename_cooldown_hours"] = -1 }},
		{"duplicate reserved name", func(d map[string]any) { obj(d, "names")["reserved"] = []any{"admin", "admin"} }},
		{"setting with no allowed values", func(d map[string]any) { setting(d, "difficulty")["allowed"] = []any{} }},
		{"setting without default", func(d map[string]any) { delete(setting(d, "difficulty"), "default") }},
		{"setting with non-string value", func(d map[string]any) { setting(d, "difficulty")["allowed"] = []any{"normal", 2} }},
		{"setting with unknown field", func(d map[string]any) { setting(d, "difficulty")["min"] = 1 }},
		{"setting name with capitals", func(d map[string]any) {
			obj(obj(d, "lobby"), "settings")["Difficulty"] = map[string]any{"allowed": []any{"a"}, "default": "a"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(draft, &doc); err != nil {
				t.Fatalf("unmarshal draft: %v", err)
			}
			tc.mutate(doc)
			raw, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			issues, err := schema.Validate(compiled, raw)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if len(issues) == 0 {
				t.Errorf("schema accepted %s", raw)
			}
		})
	}
}

func obj(doc map[string]any, key string) map[string]any { return doc[key].(map[string]any) }

func setting(doc map[string]any, name string) map[string]any {
	return obj(obj(obj(doc, "lobby"), "settings"), name)
}
