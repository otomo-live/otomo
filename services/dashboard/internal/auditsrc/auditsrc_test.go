package auditsrc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstream is a minimal audit feed: an ordered slice, an integer cursor, and the request
// bits a test wants to inspect.
type upstream struct {
	t       *testing.T
	entries []Entry
	status  int

	mu    sync.Mutex
	auth  string
	query url.Values
	other http.Header
	hits  int
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.hits++
	u.auth = r.Header.Get("Authorization")
	u.query = r.URL.Query()
	u.other = r.Header.Clone()

	if u.status != 0 {
		w.WriteHeader(u.status)
		return
	}

	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, _ = strconv.Atoi(raw)
	}
	start := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		start, _ = strconv.Atoi(raw)
	}
	if start > len(u.entries) {
		start = len(u.entries)
	}
	end := start + limit
	if end > len(u.entries) {
		end = len(u.entries)
	}

	body := map[string]any{"entries": u.entries[start:end], "next_cursor": nil}
	if end < len(u.entries) {
		body["next_cursor"] = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (u *upstream) snapshot() (auth string, query url.Values, other http.Header, hits int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.auth, u.query, u.other, u.hits
}

func entry(id int64, at time.Time) Entry {
	return Entry{
		ID:        id,
		At:        at,
		ActorID:   "staff-" + strconv.FormatInt(id, 10),
		ActorName: "Staff " + strconv.FormatInt(id, 10),
		Source:    "config",
		Action:    "update",
		Target:    "service/" + strconv.FormatInt(id, 10),
		Details:   json.RawMessage(`{"n":` + strconv.FormatInt(id, 10) + `}`),
	}
}

// TestFetchForwardsOnlyTheToken is the credential rule: the caller's bearer token is
// forwarded verbatim, and nothing else the inbound request carried travels with it.
func TestFetchForwardsOnlyTheToken(t *testing.T) {
	up := &upstream{t: t, entries: []Entry{entry(1, time.Now())}}
	srv := httptest.NewServer(up)
	defer srv.Close()

	c := New("config", srv.URL, "/audit", nil, time.Second)
	page, err := c.Fetch(context.Background(), "tok-123", Query{Limit: 50})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(page.Entries))
	}
	auth, _, other, hits := up.snapshot()
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits)
	}
	if auth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", auth)
	}
	// The transport's own headers are fine; what must not appear is any ambient header an
	// inbound call might have carried.
	if other.Get("X-Secret") != "" || other.Get("Cookie") != "" {
		t.Errorf("ambient headers leaked: %v", other)
	}
}

// TestFetchQueryParameters pins the exact upstream query the ticket defines.
func TestFetchQueryParameters(t *testing.T) {
	up := &upstream{t: t}
	srv := httptest.NewServer(up)
	defer srv.Close()

	c := New("config", srv.URL, "/api/admin/config/audit", nil, time.Second)
	_, err := c.Fetch(context.Background(), "tok", Query{
		Limit:  25,
		Cursor: "abc",
		Actor:  "staff-7",
		From:   "2024-01-02T03:04:05Z",
		To:     "2024-01-02T04:04:05Z",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	_, query, _, _ := up.snapshot()
	want := map[string]string{
		"limit":  "25",
		"cursor": "abc",
		"actor":  "staff-7",
		"from":   "2024-01-02T03:04:05Z",
		"to":     "2024-01-02T04:04:05Z",
	}
	for k, v := range want {
		if got := query.Get(k); got != v {
			t.Errorf("query %s = %q, want %q", k, got, v)
		}
	}
	if got := query.Get("action"); got != "" {
		t.Errorf("action = %q, want absent", got)
	}
}

// TestFetchPropagatesAuthStatus makes the caller's access failure the caller's status.
func TestFetchPropagatesAuthStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			up := &upstream{t: t, status: status}
			srv := httptest.NewServer(up)
			defer srv.Close()

			c := New("admin-auth", srv.URL, "/admin-auth/audit", nil, time.Second)
			_, err := c.Fetch(context.Background(), "tok", Query{Limit: 10})
			var se *StatusError
			if !errors.As(err, &se) || se.Status != status {
				t.Fatalf("Fetch error = %v, want StatusError %d", err, status)
			}
		})
	}
}

// TestFetchOtherStatusIsPlainError keeps 5xx and friends out of the auth path.
func TestFetchOtherStatusIsPlainError(t *testing.T) {
	up := &upstream{t: t, status: http.StatusInternalServerError}
	srv := httptest.NewServer(up)
	defer srv.Close()

	c := New("config", srv.URL, "/audit", nil, time.Second)
	_, err := c.Fetch(context.Background(), "tok", Query{Limit: 10})
	if err == nil {
		t.Fatal("Fetch accepted a 500")
	}
	var se *StatusError
	if errors.As(err, &se) {
		t.Fatalf("500 became StatusError %d; it must be a per-source error", se.Status)
	}
}

// TestFetchStrictDecode rejects a body that is not the documented shape rather than
// silently dropping what it cannot understand.
func TestFetchStrictDecode(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unknown field", `{"entries":[],"next_cursor":null,"extra":1}`},
		{"not json", `not json`},
		{"trailing value", `{"entries":[]}{"entries":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c := New("config", srv.URL, "/audit", nil, time.Second)
			if _, err := c.Fetch(context.Background(), "tok", Query{Limit: 10}); err == nil {
				t.Fatal("Fetch accepted a malformed page")
			}
		})
	}
}

// TestFetchCapsBody keeps a runaway upstream from making the Dashboard allocate.
func TestFetchCapsBody(t *testing.T) {
	huge := `{"entries":[],"next_cursor":null}` + strings.Repeat(" ", maxBodyBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	c := New("config", srv.URL, "/audit", nil, time.Second)
	if _, err := c.Fetch(context.Background(), "tok", Query{Limit: 10}); err == nil {
		t.Fatal("Fetch accepted a body over the cap")
	}
}

// TestFetchNullCursorIsEmpty proves the explicit-null page is not mistaken for a cursor.
func TestFetchNullCursorIsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"entries":[],"next_cursor":null}`))
	}))
	defer srv.Close()

	c := New("config", srv.URL, "/audit", nil, time.Second)
	page, err := c.Fetch(context.Background(), "tok", Query{Limit: 10})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if page.NextCursor != "" || page.Entries == nil {
		t.Errorf("page = %+v, want empty entries and no cursor", page)
	}
}

// auditObserver captures the instrumentation seam's callbacks.
type auditObserver struct {
	mu     sync.Mutex
	events []struct {
		upstream string
		result   string
	}
}

func (o *auditObserver) UpstreamRequest(upstream, result string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, struct {
		upstream string
		result   string
	}{upstream, result})
}

func (o *auditObserver) last(t *testing.T) (string, string) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.events) == 0 {
		t.Fatal("no upstream events recorded")
	}
	e := o.events[len(o.events)-1]
	return e.upstream, e.result
}

// TestObserverUpstreamNames pins each feed onto the closed metric label set: the configured
// name "admin-auth" must never leak into a label.
func TestObserverUpstreamNames(t *testing.T) {
	for _, tc := range []struct {
		configured string
		want       string
	}{
		{"config", "config_audit"},
		{"admin-auth", "admin_auth_audit"},
	} {
		t.Run(tc.configured, func(t *testing.T) {
			up := &upstream{t: t, entries: []Entry{entry(1, time.Now())}}
			srv := httptest.NewServer(up)
			defer srv.Close()

			c := New(tc.configured, srv.URL, "/audit", nil, time.Second)
			obs := &auditObserver{}
			c.Observer = obs

			if _, err := c.Fetch(context.Background(), "tok", Query{Limit: 10}); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if upstream, result := obs.last(t); upstream != tc.want || result != "ok" {
				t.Fatalf("event = %s/%s, want %s/ok", upstream, result, tc.want)
			}
		})
	}
}
