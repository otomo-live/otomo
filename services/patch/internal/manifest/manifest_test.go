package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestCanonicalIsOrderAndWhitespaceIndependent is the property the jsonb round-trip
// loses: two spellings of the same document must produce the same bytes.
func TestCanonicalIsOrderAndWhitespaceIndependent(t *testing.T) {
	a, err := Canonical([]byte(`{"b":1,"a":2}`))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	b, err := Canonical([]byte("{\n  \"a\" : 2,\n  \"b\": 1\n}"))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if string(a) != string(b) || string(a) != `{"a":2,"b":1}` {
		t.Fatalf("got %s and %s, want {\"a\":2,\"b\":1}", a, b)
	}
}

// TestCanonicalSortsNestedObjects covers recursion: sorting only the top level would
// still leave a nested object's key order to chance.
func TestCanonicalSortsNestedObjects(t *testing.T) {
	got, err := Canonical([]byte(`{"z":{"y":1,"x":2},"a":[3,{"b":1,"a":2}]}`))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	want := `{"a":[3,{"a":2,"b":1}],"z":{"x":2,"y":1}}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// TestCanonicalPreservesLargeIntegers is the reason Canonical does not call
// Value.Canonicalize: release_id is an int64, and the float64 path would round it.
func TestCanonicalPreservesLargeIntegers(t *testing.T) {
	const big = 9007199254740993 // 2^53 + 1
	got, err := Canonical([]byte(fmt.Sprintf(`{"release_id":%d}`, big)))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if want := fmt.Sprintf(`{"release_id":%d}`, big); string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// TestCanonicalIsIdempotent: canonicalizing already-canonical bytes must not move them,
// which is what makes the bootstrap seed's stored hash equal to the recomputed one.
func TestCanonicalIsIdempotent(t *testing.T) {
	first, err := Canonical([]byte(`{"b":"x","a":[1,2,{"d":true,"c":null}]}`))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	second, err := Canonical(first)
	if err != nil {
		t.Fatalf("Canonical second pass: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("not idempotent: %s then %s", first, second)
	}
}

// TestCanonicalRejectsDuplicateKeys: RFC 7493 forbids duplicate object names, and a
// document whose meaning depends on which duplicate wins is not canonicalizable.
func TestCanonicalRejectsDuplicateKeys(t *testing.T) {
	if _, err := Canonical([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("Canonical accepted duplicate keys")
	}
}

// TestCanonicalRejectsTrailingData guards the boundary: one manifest is exactly one
// JSON value, and a decoder that silently ignores the rest would hash the wrong bytes.
func TestCanonicalRejectsTrailingData(t *testing.T) {
	if _, err := Canonical([]byte(`{} {}`)); err == nil {
		t.Fatal("Canonical accepted trailing data")
	}
}

// TestCanonicalMatchesBootstrapBody is the cross-check against migration 00002: the
// exact body string its format(...) builds is already canonical, so Canonical returns
// it unchanged and its stored sha256 is reproduced here.
func TestCanonicalMatchesBootstrapBody(t *testing.T) {
	for _, ch := range []string{"dev", "staging", "live"} {
		var id int64 = 42
		body := fmt.Sprintf(
			`{"channel":"%s","config":{},"format":1,"min_client_version":"0.0.0","packs":[],"release_id":%d}`,
			ch, id)

		got, err := Canonical([]byte(body))
		if err != nil {
			t.Fatalf("%s: Canonical: %v", ch, err)
		}
		if string(got) != body {
			t.Errorf("%s: Canonical changed the bootstrap body\n got %s\nwant %s", ch, got, body)
		}
	}
}

// TestSetGet covers the map contract every handler relies on: a missing channel is not
// an error, it is simply absent.
func TestSetGet(t *testing.T) {
	e := &Entry{Channel: "live", ReleaseID: 7}
	s := newSet(map[string]*Entry{"live": e})

	got, ok := s.Get("live")
	if !ok || got != e {
		t.Fatalf("Get(live) = %v, %v; want the published entry", got, ok)
	}
	if _, ok := s.Get("staging"); ok {
		t.Fatal("Get(staging) reported an entry that was never published")
	}

	var empty *Set
	if _, ok := empty.Get("live"); ok {
		t.Fatal("a nil Set reported an entry")
	}
}

// TestHolderReady tracks the readiness gate from before the first publish to after it.
func TestHolderReady(t *testing.T) {
	var h Holder
	if err := h.Ready(); err == nil || !strings.Contains(err.Error(), "manifests not loaded") {
		t.Fatalf("Ready before load = %v, want \"manifests not loaded\"", err)
	}
	if h.Load() != nil {
		t.Fatal("Load before publish returned a Set")
	}

	h.publish(newSet(map[string]*Entry{"live": {Channel: "live"}}))
	if err := h.Ready(); err != nil {
		t.Fatalf("Ready after load = %v, want nil", err)
	}
	if h.Load() == nil {
		t.Fatal("Load after publish returned nil")
	}
}

// TestPublishedSetIsImmutable is the reload contract: a reader holding the old pointer
// keeps seeing the old entry after a new set is published.
func TestPublishedSetIsImmutable(t *testing.T) {
	var h Holder
	old := &Entry{Channel: "live", ReleaseID: 1, Body: []byte(`{"release_id":1}`)}
	h.publish(newSet(map[string]*Entry{"live": old}))

	snapshot := h.Load()
	h.publish(newSet(map[string]*Entry{
		"live": {Channel: "live", ReleaseID: 2, Body: []byte(`{"release_id":2}`)},
	}))

	got, ok := snapshot.Get("live")
	if !ok || got.ReleaseID != 1 {
		t.Fatalf("old snapshot now returns release %d (ok=%v), want release 1", got.ReleaseID, ok)
	}
	if fresh, _ := h.Load().Get("live"); fresh.ReleaseID != 2 {
		t.Fatalf("new snapshot returns release %d, want 2", fresh.ReleaseID)
	}
}

// TestConcurrentLoadAndSwap is the -race check that publications are complete: every
// reader that sees an entry must see a Body and its matching ETag, never a torn value.
func TestConcurrentLoadAndSwap(t *testing.T) {
	var h Holder

	makeEntry := func(i int) *Entry {
		body, err := Canonical([]byte(fmt.Sprintf(`{"release_id":%d}`, i)))
		if err != nil {
			t.Fatalf("Canonical: %v", err)
		}
		sum := sha256.Sum256(body)
		return &Entry{
			Channel:   "live",
			ReleaseID: int64(i),
			Body:      body,
			ETag:      `"` + hex.EncodeToString(sum[:]) + `"`,
		}
	}

	h.publish(newSet(map[string]*Entry{"live": makeEntry(0)}))

	const (
		readers = 8
		rounds  = 1000
	)
	var wg sync.WaitGroup

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				e, ok := h.Load().Get("live")
				if !ok || e == nil {
					t.Errorf("reader saw no live entry")
					return
				}
				sum := sha256.Sum256(e.Body)
				if got, want := e.ETag, `"`+hex.EncodeToString(sum[:])+`"`; got != want {
					t.Errorf("reader saw ETag %s for body hashing to %s", got, want)
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 1000; i++ {
			h.publish(newSet(map[string]*Entry{"live": makeEntry(i)}))
		}
	}()

	wg.Wait()
}

// Parity with services/config's schema.Canonical: floats are canonicalized (jsonb can
// re-print 1e2 as 100) and integers are not. If the two services ever disagree, Patch
// refuses releases Config published correctly.
func TestCanonicalFloatsAreNormalisedIntegersAreNot(t *testing.T) {
	a, err := Canonical([]byte(`{"x":1e2,"y":1.50,"n":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonical([]byte(`{"n":9007199254740993,"y":1.5,"x":100}`))
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
