package diff_test

import (
	"encoding/json"
	"testing"

	"github.com/otomo-live/otomo/services/config/internal/diff"
)

// changes returns the diff as its wire JSON, which is what a client sees and is easier
// to read in a failure than a []Change.
func changes(t *testing.T, from, to string) string {
	t.Helper()

	got, err := diff.Diff([]byte(from), []byte(to))
	if err != nil {
		t.Fatalf("Diff(%s, %s): %v", from, to, err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal changes: %v", err)
	}
	return string(raw)
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name     string
		from, to string
		want     string
	}{
		{
			name: "identical",
			from: `{"a":{"b":[1,2]},"c":"x"}`,
			to:   `{"a":{"b":[1,2]},"c":"x"}`,
			want: `[]`,
		},
		{
			name: "integer and float are equal",
			from: `{"a":1}`,
			to:   `{"a":1.0}`,
			want: `[]`,
		},
		{
			name: "one equals one e zero",
			from: `{"a":1}`,
			to:   `{"a":1e0}`,
			want: `[]`,
		},
		{
			name: "nested replace",
			from: `{"a":{"b":5}}`,
			to:   `{"a":{"b":7}}`,
			want: `[{"op":"replace","path":"/a/b","from":5,"to":7}]`,
		},
		{
			name: "add key",
			from: `{"a":1}`,
			to:   `{"a":1,"b":2}`,
			want: `[{"op":"add","path":"/b","to":2}]`,
		},
		{
			name: "remove key",
			from: `{"a":1,"b":2}`,
			to:   `{"a":1}`,
			want: `[{"op":"remove","path":"/b","from":2}]`,
		},
		{
			name: "add key with a false value",
			from: `{"a":1}`,
			to:   `{"a":1,"b":false}`,
			want: `[{"op":"add","path":"/b","to":false}]`,
		},
		{
			name: "remove key with a false value",
			from: `{"a":1,"b":false}`,
			to:   `{"a":1}`,
			want: `[{"op":"remove","path":"/b","from":false}]`,
		},
		{
			name: "array grows",
			from: `{"a":[1]}`,
			to:   `{"a":[1,2]}`,
			want: `[{"op":"add","path":"/a/1","to":2}]`,
		},
		{
			name: "array shrinks",
			from: `{"a":[1,2,3]}`,
			to:   `{"a":[1]}`,
			want: `[{"op":"remove","path":"/a/1","from":2},{"op":"remove","path":"/a/2","from":3}]`,
		},
		{
			name: "array element changes",
			from: `{"a":[1,2,3]}`,
			to:   `{"a":[1,9,3]}`,
			want: `[{"op":"replace","path":"/a/1","from":2,"to":9}]`,
		},
		{
			name: "array reorder is index-wise",
			from: `{"a":[1,2,3]}`,
			to:   `{"a":[3,1,2]}`,
			want: `[{"op":"replace","path":"/a/0","from":1,"to":3},` +
				`{"op":"replace","path":"/a/1","from":2,"to":1},` +
				`{"op":"replace","path":"/a/2","from":3,"to":2}]`,
		},
		{
			name: "type change object to array is one replace",
			from: `{"a":{"x":1}}`,
			to:   `{"a":[1]}`,
			want: `[{"op":"replace","path":"/a","from":{"x":1},"to":[1]}]`,
		},
		{
			name: "type change scalar to object is one replace",
			from: `{"a":1}`,
			to:   `{"a":{"x":1}}`,
			want: `[{"op":"replace","path":"/a","from":1,"to":{"x":1}}]`,
		},
		{
			name: "root type change",
			from: `[1,2]`,
			to:   `{"a":1}`,
			want: `[{"op":"replace","path":"","from":[1,2],"to":{"a":1}}]`,
		},
		{
			name: "root replace",
			from: `1`,
			to:   `2`,
			want: `[{"op":"replace","path":"","from":1,"to":2}]`,
		},
		{
			name: "keys are escaped",
			from: `{"a/b":1,"c~d":2,"plain":3}`,
			to:   `{"a/b":9,"plain":3}`,
			want: `[{"op":"replace","path":"/a~1b","from":1,"to":9},` +
				`{"op":"remove","path":"/c~0d","from":2}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := changes(t, tt.from, tt.to); got != tt.want {
				t.Errorf("Diff(%s, %s) =\n  %s\nwant\n  %s", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

// TestDiffIdenticalIsNeverNull pins the [] not null contract for a caller that renders
// the list without a nil check.
func TestDiffIdenticalIsNeverNull(t *testing.T) {
	got, err := diff.Diff([]byte(`{"a":1}`), []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got == nil {
		t.Fatal("Diff returned a nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("Diff = %v, want empty", got)
	}
}

// TestDiffOrderIsDeterministic runs the same diff many times: key order must come from
// the sorted union, not from Go's randomised map iteration.
func TestDiffOrderIsDeterministic(t *testing.T) {
	from := `{"z":1,"a":{"q":1,"b":2},"m":[1,2],"y":3}`
	to := `{"z":2,"a":{"q":2,"c":3},"m":[1,3,4],"x":4}`

	want := changes(t, from, to)
	for i := 0; i < 50; i++ {
		if got := changes(t, from, to); got != want {
			t.Fatalf("run %d =\n  %s\nwant\n  %s", i, got, want)
		}
	}
}

// TestDiffRejectsInvalidJSON keeps the error path honest: the handler hands Diff bytes
// straight from the database, and a malformed document must be reported, not silently
// treated as an empty diff.
func TestDiffRejectsInvalidJSON(t *testing.T) {
	if _, err := diff.Diff([]byte(`{`), []byte(`{}`)); err == nil {
		t.Error("Diff accepted malformed from")
	}
	if _, err := diff.Diff([]byte(`{}`), []byte(`nope`)); err == nil {
		t.Error("Diff accepted malformed to")
	}
}
