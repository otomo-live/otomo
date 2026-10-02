package rules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

const testKey = "a-service-key-that-is-long-enough-0123456789"

// fakePatch serves a server manifest and its blobs the way Patch's internal listener
// does: behind the key, with an ETag, and 304 for a matching If-None-Match.
type fakePatch struct {
	mu        sync.Mutex
	manifest  string
	blobs     map[string]string
	corrupt   bool // serve blobs with a flipped byte
	down      bool
	blobGets  int
	manifests int
	release   int
	noHeader  bool // leave X-Otomo-Release out, as a Patch before CF-3 would
	*httptest.Server
}

func newFakePatch(t *testing.T) *fakePatch {
	f := &fakePatch{blobs: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	f.publish(7, "") // a release with no session.rules
	return f
}

// publish makes release the channel head, with doc as its session.rules ("" for none).
func (f *fakePatch) publish(release int, doc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	config := "{}"
	if doc != "" {
		sum := sha256.Sum256([]byte(doc))
		sha := hex.EncodeToString(sum[:])
		f.blobs[sha] = doc
		config = fmt.Sprintf(`{"session.rules":{"sha256":%q,"size":%d,"version":1}}`, sha, len(doc))
	}
	f.manifest = fmt.Sprintf(`{"channel":"live","config":%s,"format":1,"packs":[],"release_id":%d}`, config, release)
	f.release = release
}

func (f *fakePatch) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/internal/patch/server-manifest/live":
		f.manifests++
		sum := sha256.Sum256([]byte(f.manifest))
		etag := `"` + hex.EncodeToString(sum[:]) + `"`
		w.Header().Set("ETag", etag)
		if !f.noHeader {
			w.Header().Set(ReleaseHeader, fmt.Sprint(f.release))
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, f.manifest)
	case strings.HasPrefix(r.URL.Path, "/internal/patch/blob/"):
		f.blobGets++
		doc, ok := f.blobs[strings.TrimPrefix(r.URL.Path, "/internal/patch/blob/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if f.corrupt {
			doc = strings.Replace(doc, "{", "[", 1)
		}
		_, _ = io.WriteString(w, doc)
	default:
		http.NotFound(w, r)
	}
}

func newTestLoader(f *fakePatch, key string, every time.Duration) *Loader {
	return NewLoader(LoaderConfig{
		PatchURL: f.URL, Channel: "live", Key: key, Interval: every,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func polls(l *Loader, result string) float64 {
	return testutil.ToFloat64(l.polls.WithLabelValues(result))
}

// TestANewMaxPartySizeTakesEffectWithinTheInterval is LB-1's acceptance criterion:
// publishing a new max party size changes the limit handlers read within one poll.
func TestANewMaxPartySizeTakesEffectWithinTheInterval(t *testing.T) {
	f := newFakePatch(t)
	l := newTestLoader(f, testKey, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	if got := l.Current().Party.MaxSize; got != 4 {
		t.Fatalf("before any release, max_size = %d, want the default 4", got)
	}
	f.publish(8, `{"party":{"max_size":6}}`)

	deadline := time.Now().Add(2 * time.Second)
	for l.Current().Party.MaxSize != 6 {
		if time.Now().After(deadline) {
			t.Fatalf("max_size is still %d two seconds after publishing 6", l.Current().Party.MaxSize)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUnchangedManifestIsA304: once loaded, polls send the ETag and fetch nothing more.
func TestUnchangedManifestIsA304(t *testing.T) {
	f := newFakePatch(t)
	f.publish(8, `{"party":{"max_size":5}}`)
	l := newTestLoader(f, testKey, time.Hour)

	if got := l.Poll(t.Context()); got != ResultLoaded {
		t.Fatalf("first poll = %s, want loaded", got)
	}
	for range 3 {
		if got := l.Poll(t.Context()); got != ResultNotModified {
			t.Fatalf("poll of an unchanged manifest = %s, want not_modified", got)
		}
	}
	if f.blobGets != 1 {
		t.Errorf("fetched the document %d times, want once", f.blobGets)
	}
	if got := testutil.ToFloat64(l.release); got != 8 {
		t.Errorf("session_rules_release_id = %v, want 8", got)
	}
}

// TestABadDocumentKeepsTheLastGoodRules is LB-1's other criterion, with the metric.
func TestABadDocumentKeepsTheLastGoodRules(t *testing.T) {
	f := newFakePatch(t)
	f.publish(8, `{"party":{"max_size":5}}`)
	l := newTestLoader(f, testKey, time.Hour)
	l.Poll(t.Context())

	for i, bad := range []string{
		`{"names":{"min_length":10,"max_length":5}}`,                               // cross-field check
		`{"lobby":{"settings":{"difficulty":{"allowed":["hard"],"default":"x"}}}}`, // default not allowed
		`{"party":{"max_size":"many"}}`,                                            // wrong type
	} {
		f.publish(9+i, bad)
		if got := l.Poll(t.Context()); got != ResultRejected {
			t.Errorf("poll of %s = %s, want rejected", bad, got)
		}
		if l.Current().Party.MaxSize != 5 {
			t.Fatalf("a bad document replaced the rules: max_size %d", l.Current().Party.MaxSize)
		}
	}
	if got := polls(l, ResultRejected); got != 3 {
		t.Errorf("session_rules_polls_total{result=rejected} = %v, want 3", got)
	}

	// The same bad release is not refetched on every poll.
	gets := f.blobGets
	l.Poll(t.Context())
	if f.blobGets != gets {
		t.Error("the loader fetched a rejected document again for the same release")
	}

	// A later good release is taken.
	f.publish(20, `{"party":{"max_size":3}}`)
	if got := l.Poll(t.Context()); got != ResultLoaded || l.Current().Party.MaxSize != 3 {
		t.Errorf("good release after bad = %s, max_size %d", got, l.Current().Party.MaxSize)
	}
}

func TestAHashMismatchIsRejectedAndRetried(t *testing.T) {
	f := newFakePatch(t)
	f.publish(8, `{"party":{"max_size":5}}`)
	f.corrupt = true
	l := newTestLoader(f, testKey, time.Hour)

	if got := l.Poll(t.Context()); got != ResultRejected || l.Current().Party.MaxSize != 4 {
		t.Fatalf("corrupt blob = %s, max_size %d; want rejected and the defaults", got, l.Current().Party.MaxSize)
	}
	f.corrupt = false
	if got := l.Poll(t.Context()); got != ResultLoaded || l.Current().Party.MaxSize != 5 {
		t.Errorf("after the blob was fixed = %s, max_size %d", got, l.Current().Party.MaxSize)
	}
}

func TestPatchUnavailableOrWrongKeyKeepsTheRules(t *testing.T) {
	f := newFakePatch(t)
	f.publish(8, `{"party":{"max_size":5}}`)
	l := newTestLoader(f, testKey, time.Hour)
	l.Poll(t.Context())

	f.down = true
	if got := l.Poll(t.Context()); got != ResultError || l.Current().Party.MaxSize != 5 {
		t.Errorf("Patch down = %s, max_size %d; want error and the last good rules", got, l.Current().Party.MaxSize)
	}

	wrong := newTestLoader(f, "not-the-key-but-long-enough-to-look-like-one", time.Hour)
	f.down = false
	if got := wrong.Poll(t.Context()); got != ResultError || wrong.Current().Party.MaxSize != 4 {
		t.Errorf("wrong key = %s, max_size %d; want error and the defaults", got, wrong.Current().Party.MaxSize)
	}
}

// TestAReleaseWithoutSessionRulesMeansTheDefaults: taking session.rules out of a
// release puts the compiled-in defaults back in force.
func TestAReleaseWithoutSessionRulesMeansTheDefaults(t *testing.T) {
	f := newFakePatch(t)
	f.publish(8, `{"party":{"max_size":5}}`)
	l := newTestLoader(f, testKey, time.Hour)
	l.Poll(t.Context())

	f.publish(9, "")
	if got := l.Poll(t.Context()); got != ResultLoaded || l.Current().Party.MaxSize != 4 {
		t.Errorf("release without session.rules = %s, max_size %d; want the defaults", got, l.Current().Party.MaxSize)
	}
}

// TestHeadFollowsTheChannel is SE-8's source: the head is the release Patch serves,
// from X-Otomo-Release, whether or not its rules changed, and it moves back down on a
// rollback.
func TestHeadFollowsTheChannel(t *testing.T) {
	f := newFakePatch(t)
	l := newTestLoader(f, testKey, time.Hour)
	if l.Head() != 0 {
		t.Errorf("head before any poll = %d, want 0", l.Head())
	}
	l.Poll(t.Context())
	if l.Head() != 7 {
		t.Errorf("head = %d, want 7", l.Head())
	}
	f.publish(9, `{"party":{"max_size":6}}`)
	l.Poll(t.Context())
	if l.Head() != 9 {
		t.Errorf("head after a publish = %d, want 9", l.Head())
	}
	f.publish(8, `{"party":{"max_size":6}}`) // a rollback with the same rules
	l.Poll(t.Context())
	if l.Head() != 8 {
		t.Errorf("head after a rollback = %d, want 8", l.Head())
	}

	// Without the header the manifest's own release_id is used.
	f.noHeader = true
	f.publish(10, "")
	l.Poll(t.Context())
	if l.Head() != 10 {
		t.Errorf("head without the header = %d, want 10", l.Head())
	}

	// Patch down: the last head is kept.
	f.down = true
	l.Poll(t.Context())
	if l.Head() != 10 {
		t.Errorf("head with Patch down = %d, want the last one, 10", l.Head())
	}
}

// TestRefreshPollsAtMostEveryFewSeconds: a mismatched client makes the loader ask Patch
// again at once, but a flood of them asks once per RefreshMin.
func TestRefreshPollsAtMostEveryFewSeconds(t *testing.T) {
	f := newFakePatch(t)
	l := newTestLoader(f, testKey, time.Hour)
	l.Poll(t.Context())
	f.publish(8, "")

	// The last poll was just now: no new poll yet.
	if got := l.Refresh(t.Context()); got != 7 {
		t.Errorf("Refresh right after a poll = %d, want 7", got)
	}
	l.mu.Lock()
	l.lastPoll = time.Now().Add(-RefreshMin)
	l.mu.Unlock()
	before := f.manifests
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { l.Refresh(t.Context()) })
	}
	wg.Wait()
	if f.manifests-before != 1 {
		t.Errorf("%d polls for 20 refreshes, want 1", f.manifests-before)
	}
	if l.Head() != 8 {
		t.Errorf("head after the refresh = %d, want 8", l.Head())
	}
}
