package rules

import (
	"os"
	"strings"
	"testing"
)

func TestParseTheSeed(t *testing.T) {
	raw, err := os.ReadFile(seedDraft)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(raw)
	if err != nil {
		t.Fatalf("the seeded document does not parse: %v", err)
	}
	if r.Party.MaxSize != 4 || r.Names.MaxLength != 16 {
		t.Errorf("parsed seed = %+v", r)
	}
}

// TestParseKeepsDefaultsForWhatIsMissing: the schema's promise, a value the document
// leaves out keeps its compiled-in default.
func TestParseKeepsDefaultsForWhatIsMissing(t *testing.T) {
	r, err := Parse([]byte(`{"party":{"max_size":6}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Party.MaxSize != 6 {
		t.Errorf("max_size = %d, want 6", r.Party.MaxSize)
	}
	if r.Names.MinLength != 3 || r.Friends.MaxFriends != 200 || len(r.Lobby.Settings) != 2 {
		t.Errorf("a missing section lost its defaults: %+v", r)
	}
	if _, err := r.Names.Check("Tanuki"); err != nil {
		t.Errorf("the parsed name rules are not compiled: %v", err)
	}
}

func TestParseReplacesLobbySettingsWhole(t *testing.T) {
	r, err := Parse([]byte(`{"lobby":{"settings":{"map":{"allowed":["a","b"],"default":"b"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Lobby.Settings) != 1 || r.Lobby.Settings["map"].Default != "b" {
		t.Errorf("settings = %+v, want only the document's", r.Lobby.Settings)
	}
}

// TestParseRejects covers the schema's own rules and the two cross-field checks it
// cannot express.
func TestParseRejects(t *testing.T) {
	for _, tt := range []struct{ doc, want string }{
		{`{"names":{"min_length":10,"max_length":5}}`, "below names.min_length"},
		{`{"lobby":{"settings":{"expedition":{"allowed":["a"],"default":"b"}}}}`, "not one of its allowed values"},
		{`{"lobby":{"settings":{"expedition":{"allowed":[],"default":"a"}}}}`, "no allowed values"},
		{`{"lobby":{"settings":{"Bad-Key":{"allowed":["a"],"default":"a"}}}}`, "not a valid setting name"},
		{`{"party":{"max_size":0}}`, "party.max_size"},
		{`{"party":{"max_size":17}}`, "party.max_size"},
		{`{"names":{"allowed_pattern":"^[a-z"}}`, "allowed_pattern"},
		{`{"names":{"rename_cooldown_hours":-1}}`, "rename_cooldown_hours"},
		{`{"party":{"max_size":4,"colour":"red"}}`, "party"},
		{`{"shop":{}}`, "unknown section"},
		{`{"party":{"max_size":"four"}}`, "party"},
		{`not json`, "document"},
		{`{} {}`, "trailing"},
	} {
		if _, err := Parse([]byte(tt.doc)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Parse(%s) = %v, want an error mentioning %q", tt.doc, err, tt.want)
		}
	}
}
