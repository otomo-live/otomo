// Package diff computes a structural diff between two JSON documents.
//
// The output is a JSON Patch-like list of RFC 6901 changes: objects are compared key
// by key (keys visited in sorted order, so the list is deterministic), arrays are
// compared index by index, and a change of JSON type at one location is a single
// replace rather than a remove followed by an add.
//
// Scalars are compared by JSON value, not by their source text: 1 and 1.0 are equal.
// Numbers are decoded with UseNumber and canonicalised the way schema.Canonical
// canonicalises floats, which is the rule the rest of the service hashes and stores
// documents by.
package diff

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/otomo-live/otomo/services/config/internal/schema"
)

// Change is one difference between two documents. From is set for remove and replace,
// To for add and replace, and neither is set for a no-op (which is never emitted).
type Change struct {
	Op   string
	Path string
	From any
	To   any
}

// MarshalJSON emits exactly the fields the op uses. A struct tag cannot express that,
// and omitempty would drop a legitimate false, 0, "" or null value.
func (c Change) MarshalJSON() ([]byte, error) {
	switch c.Op {
	case "add":
		return json.Marshal(struct {
			Op   string `json:"op"`
			Path string `json:"path"`
			To   any    `json:"to"`
		}{c.Op, c.Path, c.To})
	case "remove":
		return json.Marshal(struct {
			Op   string `json:"op"`
			Path string `json:"path"`
			From any    `json:"from"`
		}{c.Op, c.Path, c.From})
	default:
		return json.Marshal(struct {
			Op   string `json:"op"`
			Path string `json:"path"`
			From any    `json:"from"`
			To   any    `json:"to"`
		}{c.Op, c.Path, c.From, c.To})
	}
}

// Diff returns the changes that turn from into to. Both are complete JSON documents.
// When they are equal the result is an empty, non-nil slice.
func Diff(from, to []byte) ([]Change, error) {
	a, err := decode(from)
	if err != nil {
		return nil, err
	}
	b, err := decode(to)
	if err != nil {
		return nil, err
	}

	changes := make([]Change, 0)
	diff("", a, b, &changes)
	return changes, nil
}

// decode parses raw into any, keeping numbers as json.Number so their literal text is
// available for canonical comparison.
func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// diff appends the changes between a and b at path to out.
func diff(path string, a, b any, out *[]Change) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, Change{Op: "replace", Path: path, From: a, To: b})
			return
		}
		for _, k := range unionKeys(av, bv) {
			from, fromOK := av[k]
			to, toOK := bv[k]
			switch {
			case fromOK && toOK:
				diff(join(path, k), from, to, out)
			case fromOK:
				*out = append(*out, Change{Op: "remove", Path: join(path, k), From: from})
			default:
				*out = append(*out, Change{Op: "add", Path: join(path, k), To: to})
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, Change{Op: "replace", Path: path, From: a, To: b})
			return
		}
		for i := 0; i < max(len(av), len(bv)); i++ {
			index := path + "/" + strconv.Itoa(i)
			switch {
			case i >= len(av):
				*out = append(*out, Change{Op: "add", Path: index, To: bv[i]})
			case i >= len(bv):
				*out = append(*out, Change{Op: "remove", Path: index, From: av[i]})
			default:
				diff(index, av[i], bv[i], out)
			}
		}
	default:
		if scalarEqual(a, b) {
			return
		}
		*out = append(*out, Change{Op: "replace", Path: path, From: a, To: b})
	}
}

// unionKeys returns the keys of both maps in sorted order, so the diff does not depend
// on Go's map iteration order.
func unionKeys(a, b map[string]any) []string {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// scalarEqual reports whether two non-container JSON values are equal. Numbers are
// compared by canonical text, so 1 and 1.0 are equal while 1 and 2 are not.
func scalarEqual(a, b any) bool {
	switch av := a.(type) {
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && canonicalNumber(av) == canonicalNumber(bv)
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	default:
		return false
	}
}

// canonicalNumber returns the canonical text of a number. A number that cannot be
// canonicalised falls back to its source text, which at worst makes two equal numbers
// compare unequal rather than the reverse.
func canonicalNumber(n json.Number) string {
	canonical, err := schema.Canonical([]byte(n.String()))
	if err != nil {
		return n.String()
	}
	return string(canonical)
}

// join appends key to path as an RFC 6901 reference token, escaping ~ as ~0 and / as
// ~1. Array indices are already safe.
func join(path, key string) string {
	key = strings.ReplaceAll(key, "~", "~0")
	key = strings.ReplaceAll(key, "/", "~1")
	return path + "/" + key
}
