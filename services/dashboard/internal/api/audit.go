package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/auditsrc"
	"github.com/otomo-live/otomo/services/dashboard/internal/auth"
)

// auditPath is the external path for GET /audit.
const auditPath = "/api/admin/dashboard/audit"

// auditPattern is the ServeMux pattern for auditPath, compared by For while the mux is
// assembled.
const auditPattern = http.MethodGet + " " + auditPath

const (
	// defaultAuditLimit is how many merged entries a request without an explicit limit
	// returns.
	defaultAuditLimit = 50
	// maxAuditLimit is the hard cap on a single /audit page, matching the upstreams' own
	// limit ceiling.
	maxAuditLimit = 200
)

// The upstream feeds' configured names. They are also the values accepted by the
// `source` parameter and the names reported in `degraded`.
const (
	auditSourceConfig    = "config"
	auditSourceAdminAuth = "admin-auth"
	auditSourceSession   = "session"
)

// auditResponse is the wire body GET /audit returns. Degraded names the feeds whose fetch
// failed and were left out of this page; NextCursor is null only when every included feed
// is exhausted.
type auditResponse struct {
	Entries    []auditsrc.Entry `json:"entries"`
	NextCursor *string          `json:"next_cursor"`
	Degraded   []string         `json:"degraded"`
}

// auditResume is one feed's position inside the composite cursor. Cursor is the opaque
// upstream cursor the feed was paged from, LastID is the id of the last entry of that
// page already returned (0 when none), and Done records that the feed has no more.
//
// Resuming refetches the page from Cursor and drops every entry with id >= LastID. That
// is exact because each feed is ordered by id descending, and it stays exact when new
// rows arrive in between: on the head page (empty Cursor) new rows land on top with
// larger ids, which a positional count would have mistaken for returned ones.
type auditResume struct {
	Cursor string `json:"cursor"`
	LastID int64  `json:"last_id,omitempty"`
	Done   bool   `json:"done,omitempty"`
}

// auditCursor is the base64url JSON composite cursor: one resume position per feed. A
// field is present only for a feed this paging session has included.
type auditCursor struct {
	Config    *auditResume `json:"config,omitempty"`
	AdminAuth *auditResume `json:"admin-auth,omitempty"`
	Session   *auditResume `json:"session,omitempty"`
}

// forSource returns the resume position for a configured feed name, or nil when the cursor
// carries none (a fresh start for that feed).
func (c auditCursor) forSource(name string) *auditResume {
	switch name {
	case auditSourceConfig:
		return c.Config
	case auditSourceAdminAuth:
		return c.AdminAuth
	case auditSourceSession:
		return c.Session
	default:
		return nil
	}
}

// setSource records a feed's new resume position.
func (c *auditCursor) setSource(name string, r *auditResume) {
	switch name {
	case auditSourceConfig:
		c.Config = r
	case auditSourceAdminAuth:
		c.AdminAuth = r
	case auditSourceSession:
		c.Session = r
	}
}

// encodeAuditCursor renders the composite cursor as base64url with no padding.
func encodeAuditCursor(c auditCursor) (string, error) {
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

// decodeAuditCursor parses a composite cursor, rejecting anything that is not the
// documented shape. A malformed cursor is a 400 rather than an ignored parameter, so a
// client can tell paging is broken instead of silently looping over page one.
func decodeAuditCursor(raw string) (auditCursor, error) {
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return auditCursor{}, fmt.Errorf("cursor is not base64url: %w", err)
	}
	var c auditCursor
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return auditCursor{}, fmt.Errorf("cursor is not a JSON object: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return auditCursor{}, errors.New("cursor has trailing data")
	}
	for _, r := range []*auditResume{c.Config, c.AdminAuth} {
		if r != nil && r.LastID < 0 {
			return auditCursor{}, errors.New("cursor last_id must not be negative")
		}
	}
	return c, nil
}

// auditSourceState is one feed's in-progress position in the streaming merge. buf holds
// entries fetched from the current upstream page and not yet returned; pageCursor
// identifies that page and lastID is the id of the last entry returned from it (or the
// incoming position). (pageCursor, lastID) is therefore the exact place to resume when
// the page has entries left, and nextCursor is where to resume when it is exhausted.
type auditSourceState struct {
	name     string
	client   *auditsrc.Client
	incoming *auditResume

	loaded     bool
	exhausted  bool
	pageCursor string
	lastID     int64
	buf        []auditsrc.Entry
	nextCursor string
	err        error
}

// fetch loads one page from cursor into buf, dropping every entry with id >= lastID
// (already returned, or newer than this paging session). A page that is dropped entirely
// advances to its next cursor; running off the end of the feed leaves it exhausted.
func (s *auditSourceState) fetch(ctx context.Context, token string, q auditsrc.Query, cursor string, lastID int64) error {
	for {
		q.Cursor = cursor
		page, err := s.client.Fetch(ctx, token, q)
		if err != nil {
			return err
		}
		rest := page.Entries
		if lastID > 0 {
			for len(rest) > 0 && rest[0].ID >= lastID {
				rest = rest[1:]
			}
		}
		if len(rest) > 0 {
			s.loaded = true
			s.pageCursor = cursor
			s.lastID = lastID
			s.buf = rest
			s.nextCursor = page.NextCursor
			return nil
		}
		if page.NextCursor == "" {
			s.loaded = true
			s.exhausted = true
			return nil
		}
		cursor = page.NextCursor
	}
}

// start loads the feed from its incoming resume position.
func (s *auditSourceState) start(ctx context.Context, token string, q auditsrc.Query) error {
	cursor, lastID := "", int64(0)
	if s.incoming != nil {
		cursor, lastID = s.incoming.Cursor, s.incoming.LastID
	}
	return s.fetch(ctx, token, q, cursor, lastID)
}

// refill loads the feed's next page into an empty buffer.
func (s *auditSourceState) refill(ctx context.Context, token string, q auditsrc.Query) error {
	if s.nextCursor == "" {
		s.exhausted = true
		return nil
	}
	return s.fetch(ctx, token, q, s.nextCursor, 0)
}

// auditBefore is the merge order: `at` descending, then configured source name ascending,
// then id descending. It is a total order consistent with each feed's own page order, so a
// streaming k-way merge over the feeds produces the global order.
func auditBefore(a auditsrc.Entry, aName string, b auditsrc.Entry, bName string) bool {
	if !a.At.Equal(b.At) {
		return a.At.After(b.At)
	}
	if aName != bName {
		return aName < bName
	}
	return a.ID > b.ID
}

// auditAuthStatus returns the status an included feed's refusal should be answered with,
// preferring 401 and otherwise 403. Zero means no feed refused the caller's token.
func auditAuthStatus(states []auditSourceState) int {
	status := 0
	for i := range states {
		var se *auditsrc.StatusError
		if errors.As(states[i].err, &se) {
			if se.Status == http.StatusUnauthorized {
				return http.StatusUnauthorized
			}
			status = http.StatusForbidden
		}
	}
	return status
}

// cloneResume copies a resume position for the next cursor, clearing Done: a feed that
// failed is not exhausted, it is retried.
func cloneResume(r *auditResume) *auditResume {
	if r == nil {
		return &auditResume{}
	}
	cp := *r
	cp.Done = false
	return &cp
}

// audit answers GET /api/admin/dashboard/audit: a page merged from each included feed. One
// page is fetched from every included feed concurrently with the caller's own token, and
// the feeds are then k-way merged by `at` descending (ties broken by source name, then id
// descending) until the requested limit is reached. A feed whose page runs out mid-merge
// is refilled from its next cursor before an older entry from another feed can be chosen,
// so the global order holds even when a page boundary falls inside a group of equal
// timestamps.
//
// A feed that refuses the token (401/403) is propagated as that status, because the token
// is the caller's and the merged view must not be wider than the caller's access. Any
// other single-feed failure degrades: the other feed's entries are returned with the
// failed feed's name in `degraded`. When every included feed fails the request is a 502.
func (h *Handlers) audit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	source := q.Get("source")
	switch source {
	case "", auditSourceConfig, auditSourceAdminAuth, auditSourceSession:
	default:
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "source must be config, admin-auth, session or empty")
		return
	}

	limit := defaultAuditLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxAuditLimit {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "limit must be between 1 and 200")
			return
		}
		limit = n
	}

	from := q.Get("from")
	if from != "" {
		if _, err := time.Parse(time.RFC3339, from); err != nil {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "from must be an RFC3339 timestamp")
			return
		}
	}
	to := q.Get("to")
	if to != "" {
		if _, err := time.Parse(time.RFC3339, to); err != nil {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "to must be an RFC3339 timestamp")
			return
		}
	}

	state := auditCursor{}
	if raw := q.Get("cursor"); raw != "" {
		parsed, err := decodeAuditCursor(raw)
		if err != nil {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "cursor is malformed")
			return
		}
		state = parsed
	}

	clients := make(map[string]*auditsrc.Client, len(h.Audit))
	for _, c := range h.Audit {
		clients[c.Name()] = c
	}

	// Session's feed (SE-7) joins the merge when it is configured; Config's and
	// admin-auth's are always included, and a missing one is degraded as before.
	names := []string{auditSourceConfig, auditSourceAdminAuth}
	if clients[auditSourceSession] != nil {
		names = append(names, auditSourceSession)
	}
	if source != "" {
		names = []string{source}
	}

	token := auth.TokenFrom(r.Context())
	query := auditQuery(q, limit)

	states := make([]auditSourceState, len(names))
	for i, name := range names {
		st := state.forSource(name)
		if st != nil && st.Done {
			states[i] = auditSourceState{name: name, loaded: true, exhausted: true}
			continue
		}
		c := clients[name]
		if c == nil {
			states[i] = auditSourceState{name: name, incoming: st, err: fmt.Errorf("audit source %s is not configured", name)}
			continue
		}
		states[i] = auditSourceState{name: name, client: c, incoming: st}
	}

	// The first page of each feed is fetched concurrently; refills below are sequential
	// because they only happen once a feed runs out while the merge still needs entries.
	var wg sync.WaitGroup
	for i := range states {
		s := &states[i]
		if s.client == nil || s.exhausted {
			continue
		}
		wg.Add(1)
		go func(s *auditSourceState) {
			defer wg.Done()
			s.err = s.start(r.Context(), token, query)
		}(s)
	}
	wg.Wait()

	if status := auditAuthStatus(states); status != 0 {
		WriteError(w, r, status, "upstream_error", "the audit upstream refused the caller's token")
		return
	}

	var merged []mergedAudit
	for len(merged) < limit {
		// Give every feed that has run dry a fresh page before comparing heads, so no
		// older entry is chosen while a feed still holds newer, unfetched ones.
		for i := range states {
			s := &states[i]
			for s.err == nil && !s.exhausted && len(s.buf) == 0 {
				if err := s.refill(r.Context(), token, query); err != nil {
					s.err = err
				}
			}
		}

		best := -1
		for i := range states {
			s := &states[i]
			if s.err != nil || s.exhausted || len(s.buf) == 0 {
				continue
			}
			if best == -1 || auditBefore(s.buf[0], s.name, states[best].buf[0], states[best].name) {
				best = i
			}
		}
		if best == -1 {
			break
		}
		s := &states[best]
		merged = append(merged, mergedAudit{source: s.name, entry: s.buf[0]})
		s.lastID = s.buf[0].ID
		s.buf = s.buf[1:]
	}

	// A refusal discovered while refilling outranks anything already merged.
	if status := auditAuthStatus(states); status != 0 {
		WriteError(w, r, status, "upstream_error", "the audit upstream refused the caller's token")
		return
	}

	degraded := []string{}
	failed := 0
	for i := range states {
		if states[i].err != nil {
			degraded = append(degraded, states[i].name)
			failed++
		}
	}
	if failed == len(states) {
		WriteError(w, r, http.StatusBadGateway, "upstream_error", "no audit upstream could answer")
		return
	}

	entries := make([]auditsrc.Entry, len(merged))
	for i, m := range merged {
		entries[i] = m.entry
	}

	next := auditCursor{}
	pending := false
	for i := range states {
		s := &states[i]
		var resume *auditResume
		switch {
		case s.err != nil && !s.loaded:
			// The feed never answered; keep the caller's position so a retry resumes it.
			resume = cloneResume(s.incoming)
			pending = true
		case s.exhausted:
			resume = &auditResume{Done: true}
		case len(s.buf) > 0:
			resume = &auditResume{Cursor: s.pageCursor, LastID: s.lastID}
			pending = true
		case s.nextCursor != "":
			// LastID is redundant with a keyset upstream cursor, and harmless: it
			// only ever drops entries already returned.
			resume = &auditResume{Cursor: s.nextCursor, LastID: s.lastID}
			pending = true
		default:
			resume = &auditResume{Done: true}
		}
		next.setSource(s.name, resume)
	}

	var nextCursor *string
	if pending {
		encoded, err := encodeAuditCursor(next)
		if err != nil {
			WriteError(w, r, http.StatusInternalServerError, "internal_error", "could not encode the next cursor")
			return
		}
		nextCursor = &encoded
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(auditResponse{Entries: entries, NextCursor: nextCursor, Degraded: degraded})
}

// mergedAudit pairs an entry with the configured feed it came from, which the merge needs
// for its tie-break and attribution but which the entry itself must not carry.
type mergedAudit struct {
	source string
	entry  auditsrc.Entry
}

// auditQuery builds the upstream query. The cursor is filled in per feed by the state
// machine; the filters and limit are shared.
func auditQuery(q map[string][]string, limit int) auditsrc.Query {
	get := func(key string) string {
		if v := q[key]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	return auditsrc.Query{Limit: limit, Actor: get("actor"), From: get("from"), To: get("to")}
}
