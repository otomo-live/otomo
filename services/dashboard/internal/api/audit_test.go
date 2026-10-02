package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/auditsrc"
	"github.com/otomo-live/otomo/services/dashboard/internal/auth"
)

// fakeFeed is a stand-in audit upstream. Like the real feeds it pages by id
// descending with a keyset cursor (the id to continue below).
type fakeFeed struct {
	name       string
	entries    []auditsrc.Entry
	failStatus int

	mu      sync.Mutex
	hits    int
	auths   []string
	headers []http.Header
	queries []url.Values
}

func (f *fakeFeed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits++
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.headers = append(f.headers, r.Header.Clone())
	f.queries = append(f.queries, r.URL.Query())
	f.mu.Unlock()

	if f.failStatus != 0 {
		w.WriteHeader(f.failStatus)
		return
	}

	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, _ = strconv.Atoi(raw)
	}
	// A keyset cursor like the real feeds' ({"before": id}): the next page is the
	// entries with id below the cursor, so rows added at the head never shift it.
	f.mu.Lock()
	entries := append([]auditsrc.Entry(nil), f.entries...)
	f.mu.Unlock()
	start := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		before, _ := strconv.ParseInt(raw, 10, 64)
		start = len(entries)
		for i, e := range entries {
			if e.ID < before {
				start = i
				break
			}
		}
	}
	end := start + limit
	if end > len(entries) {
		end = len(entries)
	}

	body := map[string]any{"entries": entries[start:end], "next_cursor": nil}
	if end < len(entries) {
		body["next_cursor"] = strconv.FormatInt(entries[end-1].ID, 10)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeFeed) stats() (hits int, auths []string, headers []http.Header, queries []url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, append([]string(nil), f.auths...), append([]http.Header(nil), f.headers...), append([]url.Values(nil), f.queries...)
}

// newFeed starts a fake feed and returns it with the client the handler will hold.
func newFeed(t *testing.T, name string, entries []auditsrc.Entry, failStatus int) (*fakeFeed, *auditsrc.Client) {
	t.Helper()
	f := &fakeFeed{name: name, entries: entries, failStatus: failStatus}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, auditsrc.New(name, srv.URL, "/audit", nil, time.Second)
}

// auditEntries builds a newest-first list for one feed: ids are assigned in descending
// order, and timestamps come from ats (already paired with the ids).
func auditEntries(source string, ats []time.Time, ids []int64) []auditsrc.Entry {
	out := make([]auditsrc.Entry, len(ats))
	for i := range ats {
		out[i] = auditsrc.Entry{
			ID:        ids[i],
			At:        ats[i],
			ActorID:   source + "-actor",
			ActorName: source + " actor",
			Source:    source,
			Action:    "update",
			Target:    "t",
			Details:   json.RawMessage(`{}`),
		}
	}
	sortAuditEntries(out)
	return out
}

// sortAuditEntries puts a feed's page in the order its own API promises and the merge
// consumes: newest first, id descending on a tie.
func sortAuditEntries(entries []auditsrc.Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].At.Equal(entries[j].At) {
			return entries[i].At.After(entries[j].At)
		}
		return entries[i].ID > entries[j].ID
	})
}

type namedAudit struct {
	source string
	entry  auditsrc.Entry
}

// globalAuditOrder is the order the handler must produce: `at` descending, source name
// ascending, id descending.
func globalAuditOrder(feeds map[string][]auditsrc.Entry) []namedAudit {
	var all []namedAudit
	for name, entries := range feeds {
		for _, e := range entries {
			all = append(all, namedAudit{source: name, entry: e})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if !a.entry.At.Equal(b.entry.At) {
			return a.entry.At.After(b.entry.At)
		}
		if a.source != b.source {
			return a.source < b.source
		}
		return a.entry.ID > b.entry.ID
	})
	return all
}

func runAudit(t *testing.T, h *Handlers, token, query string) (*httptest.ResponseRecorder, auditResponse) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, auditPath+"?"+query, nil)
	r = r.WithContext(auth.WithToken(r.Context(), token))
	w := httptest.NewRecorder()
	h.audit(w, r)

	var body auditResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not JSON: %v (%s)", err, w.Body.String())
		}
	}
	return w, body
}

// TestAuditMergeInterleavesOnePage is the basic shape: one page of three merged from two
// feeds is the globally-ordered prefix.
func TestAuditMergeInterleavesOnePage(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	config := auditEntries("config", []time.Time{
		base.Add(5 * time.Second), base.Add(3 * time.Second), base.Add(1 * time.Second),
	}, []int64{30, 20, 10})
	admin := auditEntries("admin-auth", []time.Time{
		base.Add(4 * time.Second), base.Add(2 * time.Second), base.Add(0 * time.Second),
	}, []int64{40, 25, 5})

	_, cc := newFeed(t, "config", config, 0)
	_, ac := newFeed(t, "admin-auth", admin, 0)
	h := &Handlers{Audit: []*auditsrc.Client{cc, ac}}

	w, body := runAudit(t, h, "tok", "limit=3")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	want := globalAuditOrder(map[string][]auditsrc.Entry{"config": config, "admin-auth": admin})[:3]
	if len(body.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(body.Entries))
	}
	for i, e := range body.Entries {
		if e.ID != want[i].entry.ID {
			t.Errorf("entry %d id = %d, want %d", i, e.ID, want[i].entry.ID)
		}
	}
}

// TestAuditWalkIsExact is the property: walking with a fixed limit until next_cursor is
// null yields every entry of both feeds exactly once, in the global order (`at`
// descending, source name, then id descending), even when timestamps tie. It is run
// several times with fresh random data and several limits.
//
// Equal timestamps are the hard case: the merge must refill a feed whose page boundary
// falls inside a run of equal `at` values before it can order those entries against the
// other feed, or a newer entry can surface on a later page.
func TestAuditWalkIsExact(t *testing.T) {
	rng := rand.New(rand.NewSource(232))
	for trial := 0; trial < 12; trial++ {
		// A small time range makes equal timestamps common, which is the case that would
		// lose or duplicate an entry if the resume arithmetic were off.
		base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
		config := randomFeed(t, rng, "config", base, 1, 1+rng.Intn(40))
		admin := randomFeed(t, rng, "admin-auth", base, 1000, 1+rng.Intn(40))

		_, cc := newFeed(t, "config", config, 0)
		_, ac := newFeed(t, "admin-auth", admin, 0)
		h := &Handlers{Audit: []*auditsrc.Client{cc, ac}}

		expected := globalAuditOrder(map[string][]auditsrc.Entry{"config": config, "admin-auth": admin})

		for _, limit := range []int{1, 2, 3, 5, 7} {
			t.Run(fmt.Sprintf("trial%d-limit%d", trial, limit), func(t *testing.T) {
				cursor := ""
				var got []auditsrc.Entry
				for pages := 0; ; pages++ {
					if pages > len(expected)+5 {
						t.Fatalf("walk did not terminate after %d pages", pages)
					}
					query := "limit=" + strconv.Itoa(limit)
					if cursor != "" {
						query += "&cursor=" + cursor
					}
					w, body := runAudit(t, h, "tok", query)
					if w.Code != http.StatusOK {
						t.Fatalf("page %d status = %d, want 200 (%s)", pages, w.Code, w.Body.String())
					}
					got = append(got, body.Entries...)
					if body.NextCursor == nil {
						break
					}
					cursor = *body.NextCursor
				}

				if len(got) != len(expected) {
					t.Fatalf("walked %d entries, want %d", len(got), len(expected))
				}
				for i := range expected {
					if got[i].ID != expected[i].entry.ID || !got[i].At.Equal(expected[i].entry.At) || got[i].Source != expected[i].entry.Source {
						t.Fatalf("entry %d = {id %d, at %s, source %s}, want {id %d, at %s, source %s}",
							i, got[i].ID, got[i].At, got[i].Source,
							expected[i].entry.ID, expected[i].entry.At, expected[i].entry.Source)
					}
				}
			})
		}
	}
}

func randomFeed(t *testing.T, rng *rand.Rand, source string, base time.Time, idBase int64, n int) []auditsrc.Entry {
	t.Helper()
	ats := make([]time.Time, n)
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		ats[i] = base.Add(time.Duration(rng.Intn(6)) * time.Second)
		ids[i] = idBase + int64(i)
	}
	entries := auditEntries(source, ats, ids)
	// The real feeds (Config, admin-auth) page by id descending, and `at` is the
	// insert time, so feed order and id order agree. Renumber after sorting so the
	// fake keeps that contract: the resume position is an id.
	for i := range entries {
		entries[i].ID = idBase + int64(len(entries)-1-i)
	}
	return entries
}

// TestAuditResumeSurvivesNewRows: a row written to a feed between two page requests
// lands on top of that feed (newest, largest id). Resuming must neither repeat an
// entry already returned nor lose one, which a positional skip on the head page would.
func TestAuditResumeSurvivesNewRows(t *testing.T) {
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	var entries []auditsrc.Entry
	for i := 0; i < 6; i++ {
		entries = append(entries, auditEntries("config", []time.Time{base.Add(-time.Duration(i) * time.Minute)}, []int64{int64(100 - i)})...)
	}
	// An interleaved second feed makes the first page take one entry from each, so
	// config's head page is only partly consumed: the case a positional skip got wrong.
	var adminEntries []auditsrc.Entry
	for i := 0; i < 6; i++ {
		adminEntries = append(adminEntries, auditEntries("admin-auth", []time.Time{base.Add(-time.Duration(i)*time.Minute - 30*time.Second)}, []int64{int64(500 - i)})...)
	}
	f, cc := newFeed(t, "config", entries, 0)
	_, ac := newFeed(t, "admin-auth", adminEntries, 0)
	h := &Handlers{Audit: []*auditsrc.Client{cc, ac}}

	w, first := runAudit(t, h, "tok", "limit=2")
	if w.Code != http.StatusOK || first.NextCursor == nil {
		t.Fatalf("first page = %d, next %v", w.Code, first.NextCursor)
	}

	// Two new rows arrive at the head of the feed.
	fresh := auditEntries("config", []time.Time{base.Add(2 * time.Minute)}, []int64{102})
	fresh = append(fresh, auditEntries("config", []time.Time{base.Add(time.Minute)}, []int64{101})...)
	f.mu.Lock()
	f.entries = append(fresh, f.entries...)
	f.mu.Unlock()

	seen := map[int64]int{}
	for _, e := range first.Entries {
		seen[e.ID]++
	}
	cursor := *first.NextCursor
	for pages := 0; pages < 10; pages++ {
		w, page := runAudit(t, h, "tok", "limit=2&cursor="+cursor)
		if w.Code != http.StatusOK {
			t.Fatalf("page status = %d (%s)", w.Code, w.Body.String())
		}
		for _, e := range page.Entries {
			seen[e.ID]++
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	for id := int64(95); id <= 100; id++ {
		if seen[id] != 1 {
			t.Errorf("entry %d returned %d times, want exactly once", id, seen[id])
		}
	}
	for id := int64(495); id <= 500; id++ {
		if seen[id] != 1 {
			t.Errorf("admin-auth entry %d returned %d times, want exactly once", id, seen[id])
		}
	}
	if seen[101]+seen[102] != 0 {
		t.Errorf("rows newer than the paging session leaked into later pages: %v", seen)
	}
}

// TestAuditForwardsTokenAndNothingElse proves the handler forwards the caller's own
// credential verbatim and leaks no other inbound header.
func TestAuditForwardsTokenAndNothingElse(t *testing.T) {
	config := auditEntries("config", []time.Time{time.Now()}, []int64{1})
	f, cc := newFeed(t, "config", config, 0)
	h := &Handlers{Audit: []*auditsrc.Client{cc}}

	r := httptest.NewRequest(http.MethodGet, auditPath+"?source=config", nil)
	r.Header.Set("X-Secret", "must-not-leak")
	r.Header.Set("Cookie", "session=abc")
	r = r.WithContext(auth.WithToken(r.Context(), "caller-token"))
	w := httptest.NewRecorder()
	h.audit(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	hits, auths, headers, _ := f.stats()
	if hits != 1 || len(auths) != 1 || auths[0] != "Bearer caller-token" {
		t.Fatalf("upstream saw %d auths %v", hits, auths)
	}
	if got := headers[0].Get("X-Secret"); got != "" {
		t.Errorf("X-Secret leaked upstream: %q", got)
	}
	if got := headers[0].Get("Cookie"); got != "" {
		t.Errorf("Cookie leaked upstream: %q", got)
	}
}

// TestAuditAuthStatusPropagates carries the caller's access failure through unchanged.
func TestAuditAuthStatusPropagates(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			_, cc := newFeed(t, "config", nil, status)
			h := &Handlers{Audit: []*auditsrc.Client{cc}}

			w, _ := runAudit(t, h, "tok", "source=config")
			if w.Code != status {
				t.Fatalf("status = %d, want %d (%s)", w.Code, status, w.Body.String())
			}
		})
	}
}

// TestAuditDegradedOnOneFailure returns the live feed and names the dead one.
func TestAuditDegradedOnOneFailure(t *testing.T) {
	admin := auditEntries("admin-auth", []time.Time{time.Now()}, []int64{1})
	_, cc := newFeed(t, "config", nil, http.StatusInternalServerError)
	_, ac := newFeed(t, "admin-auth", admin, 0)
	h := &Handlers{Audit: []*auditsrc.Client{cc, ac}}

	w, body := runAudit(t, h, "tok", "limit=10")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(body.Entries) != 1 || body.Entries[0].Source != "admin-auth" {
		t.Fatalf("entries = %+v", body.Entries)
	}
	if len(body.Degraded) != 1 || body.Degraded[0] != "config" {
		t.Errorf("degraded = %v, want [config]", body.Degraded)
	}
	if body.NextCursor == nil {
		t.Error("next_cursor is null while a feed is degraded")
	}
}

// TestAuditAllFailIs502 is the degraded rule's floor.
func TestAuditAllFailIs502(t *testing.T) {
	_, cc := newFeed(t, "config", nil, http.StatusInternalServerError)
	_, ac := newFeed(t, "admin-auth", nil, http.StatusInternalServerError)
	h := &Handlers{Audit: []*auditsrc.Client{cc, ac}}

	w, _ := runAudit(t, h, "tok", "limit=10")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
}

// TestAuditSourceSelection queries only the named feed and forwards the filters.
func TestAuditSourceSelection(t *testing.T) {
	config := auditEntries("config", []time.Time{time.Now()}, []int64{1})
	cf, cc := newFeed(t, "config", config, 0)
	af, ac := newFeed(t, "admin-auth", config, 0)
	h := &Handlers{Audit: []*auditsrc.Client{cc, ac}}

	w, _ := runAudit(t, h, "tok", "source=config&actor=staff-9&from=2024-01-01T00:00:00Z&to=2024-01-02T00:00:00Z")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	chits, _, _, cqueries := cf.stats()
	ahits, _, _, _ := af.stats()
	if chits == 0 {
		t.Error("config was never queried")
	}
	if ahits != 0 {
		t.Errorf("admin-auth was queried %d times with source=config", ahits)
	}
	q := cqueries[0]
	if q.Get("actor") != "staff-9" || q.Get("from") != "2024-01-01T00:00:00Z" || q.Get("to") != "2024-01-02T00:00:00Z" {
		t.Errorf("forwarded query = %v", q)
	}
}

// TestAuditValidation covers every parameter the handler must refuse before any upstream
// call.
func TestAuditValidation(t *testing.T) {
	config := auditEntries("config", []time.Time{time.Now()}, []int64{1})
	cf, cc := newFeed(t, "config", config, 0)
	h := &Handlers{Audit: []*auditsrc.Client{cc}}

	tests := []struct {
		name  string
		query string
	}{
		{"bad source", "source=gateway"},
		{"limit zero", "limit=0"},
		{"limit 201", "limit=201"},
		{"limit not a number", "limit=abc"},
		{"bad from", "from=yesterday"},
		{"bad to", "to=never"},
		{"bad cursor", "cursor=not-base64!"},
		{"cursor not json", "cursor=" + base64Raw("nope")},
		{"cursor unknown field", "cursor=" + base64Raw(`{"else":1}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := runAudit(t, h, "tok", tt.query)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if !json.Valid(w.Body.Bytes()) {
				t.Fatalf("400 body is not JSON: %s", w.Body.String())
			}
		})
	}
	if hits, _, _, _ := cf.stats(); hits != 0 {
		t.Errorf("a rejected request reached the upstream (%d hits)", hits)
	}
}

// base64Raw is the cursor encoding the handler uses, so a malformed JSON body can be
// wrapped in a syntactically valid cursor.
func base64Raw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
