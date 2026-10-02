package rules

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// seedDraft is the session.rules document Config seeds. The defaults promise to equal it.
var seedDraft = filepath.Join("..", "..", "..", "config", "internal", "seed", "seed", "session.rules", "draft.json")

// TestDefaultsEqualTheSeed keeps the compiled-in rules and the seeded Config document in
// step: a stack with no release carrying session.rules must behave like one that has the
// seeded values.
func TestDefaultsEqualTheSeed(t *testing.T) {
	raw, err := os.ReadFile(seedDraft)
	if err != nil {
		t.Fatalf("read the seeded session.rules draft: %v", err)
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var seeded Rules
	if err := dec.Decode(&seeded); err != nil {
		t.Fatalf("the seed does not decode into Rules (a field is missing from the Go types?): %v", err)
	}
	if err := seeded.Compile(); err != nil {
		t.Fatalf("compile the seed: %v", err)
	}

	got, want := Defaults(), &seeded
	got.Names.pattern, want.Names.pattern = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Defaults() = %+v\nseed      = %+v", got, want)
	}
}

func TestNameCheck(t *testing.T) {
	names := Defaults().Names

	for _, tt := range []struct {
		in, want string
		reason   string // empty: the name is accepted
	}{
		{in: "Tanuki", want: "Tanuki"},
		{in: "  Tanuki_42 ", want: "Tanuki_42"},
		{in: "abc", want: "abc"},
		{in: "abcdefghijklmnop", want: "abcdefghijklmnop"},
		{in: "ab", reason: "3 to 16"},
		{in: "abcdefghijklmnopq", reason: "3 to 16"},
		{in: "   ", reason: "3 to 16"},
		{in: "Tan uki", reason: "not allowed"},
		{in: "Tanuki!", reason: "not allowed"},
		{in: "Tänuki", reason: "not allowed"},
		{in: "ADMIN", reason: "reserved"},
		{in: "Otomo", reason: "reserved"},
		{in: "admin1", want: "admin1"},
		{in: "\xff\xfeabc", reason: "UTF-8"},
	} {
		t.Run(tt.in, func(t *testing.T) {
			got, err := names.Check(tt.in)
			if tt.reason == "" {
				if err != nil || got != tt.want {
					t.Fatalf("Check(%q) = %q, %v; want %q, nil", tt.in, got, err, tt.want)
				}
				return
			}
			var nameErr *NameError
			if !errors.As(err, &nameErr) {
				t.Fatalf("Check(%q) = %q, %v; want a NameError", tt.in, got, err)
			}
			if !strings.Contains(nameErr.Reason, tt.reason) {
				t.Errorf("reason = %q, want it to mention %q", nameErr.Reason, tt.reason)
			}
		})
	}
}

// TestLengthCountsCharactersNotBytes: the schema measures names in code points.
func TestLengthCountsCharactersNotBytes(t *testing.T) {
	names := Defaults().Names
	names.AllowedPattern = `^\p{L}+$`
	r := &Rules{Names: names}
	if err := r.Compile(); err != nil {
		t.Fatal(err)
	}

	// Sixteen characters, 48 bytes.
	name := strings.Repeat("狸", 16)
	if _, err := r.Names.Check(name); err != nil {
		t.Errorf("Check(16 CJK characters) = %v, want accepted", err)
	}
	if _, err := r.Names.Check(name + "狸"); err == nil {
		t.Error("Check(17 CJK characters) accepted, want refused")
	}
}

// TestNamesFollowTheSource is the seam LB-1 needs: a handler that reads rules from a
// Source picks up a change without being rebuilt.
func TestNamesFollowTheSource(t *testing.T) {
	strict := Defaults()
	strict.Names.MinLength = 5
	var src Source = Static{R: strict}

	if _, err := src.Current().Names.Check("abcd"); err == nil {
		t.Error("a 4-character name passed a min_length of 5")
	}
	if _, err := (Static{}).Current().Names.Check("abcd"); err != nil {
		t.Errorf("the default source refused a 4-character name: %v", err)
	}
}

func TestCompileRejectsABadPattern(t *testing.T) {
	r := Defaults()
	r.Names.AllowedPattern = "^[a-z"
	if err := r.Compile(); err == nil {
		t.Fatal("Compile accepted an invalid pattern")
	}
}

func TestRenameCooldown(t *testing.T) {
	if got := Defaults().Names.RenameCooldown(); got != 24*time.Hour {
		t.Errorf("RenameCooldown() = %v, want 24h", got)
	}
}
