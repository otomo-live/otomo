package callback

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/otomo-live/otomo/services/allocator/internal/pool"
	"github.com/otomo-live/otomo/services/allocator/internal/testdb"
)

// The fixed ids make a bad path or body easy to read, and every UUID is already in the
// canonical lowercase Postgres returns.
const (
	callbackServer = "gs-callback"
	callbackParty  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	callbackPlayer = "11111111-1111-1111-1111-111111111111"

	// callbackKey stands in for the base64url text ReadKeyFile returns. New does not
	// reinterpret it, so any string works; the test asserts it rides the header.
	callbackKey = "c2Vzc2lvbi1hbGxvY2F0b3ItY2FsbGJhY2sta2V5" // gitleaks:allow (test fixture)
)

// capturedRequest is one request the fake Session saw. The body is kept as text so a
// test can assert the JSON without another round trip through a struct.
type capturedRequest struct {
	method        string
	path          string
	authorization string
	contentType   string
	body          string
}

// fakeSession is a Session stand-in that records every callback and answers with one
// configured status.
type fakeSession struct {
	server *httptest.Server

	mu       sync.Mutex
	status   int
	requests []capturedRequest
}

func newFakeSession(t *testing.T, status int) *fakeSession {
	t.Helper()
	f := &fakeSession{status: status}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, capturedRequest{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			contentType:   r.Header.Get("Content-Type"),
			body:          string(body),
		})
		f.mu.Unlock()
		w.WriteHeader(f.status)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// all returns a copy of the recorded requests, so a caller does not hold the lock while
// asserting.
func (f *fakeSession) all() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedRequest(nil), f.requests...)
}

func (f *fakeSession) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// discardLogger keeps the give-up warning out of the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newNotifier wires a drain at the fake Session. It passes nil metrics: these tests
// assert outbox state, and the nil-safe methods record nothing.
func newNotifier(db *pgxpool.Pool, f *fakeSession) *Notifier {
	return New(db, f.server.URL, callbackKey, &http.Client{Timeout: 5 * time.Second}, nil, discardLogger())
}

// openDB returns the migrated, truncated test database with the server the allocations
// hang off already registered.
func openDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Open(t)
	reg := pool.Registration{ServerID: callbackServer, InternalAddr: "10.0.0.1:27015", Capacity: 8}
	if err := pool.New(db, pool.Timings{}).Register(t.Context(), reg); err != nil {
		t.Fatalf("register %q: %v", callbackServer, err)
	}
	return db
}

// insertEnded writes an ended allocation that is already in the outbox and due now.
func insertEnded(t *testing.T, db *pgxpool.Pool, reason string) string {
	t.Helper()
	var id string
	err := db.QueryRow(t.Context(), `
		INSERT INTO allocation (
			allocation_id, party_id, server_id, player_ids, status,
			expires_at, ended_at, end_reason, callback_pending
		)
		VALUES (
			gen_random_uuid(), $1::text::uuid, $2, ARRAY[$3::text::uuid], 'ended',
			now(), now(), $4, true
		)
		RETURNING allocation_id::text`, callbackParty, callbackServer, callbackPlayer, reason).Scan(&id)
	if err != nil {
		t.Fatalf("insert ended allocation: %v", err)
	}
	return id
}

// insertEndedMany writes n due outbox rows and returns their ids.
func insertEndedMany(t *testing.T, db *pgxpool.Pool, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, insertEnded(t, db, "ended"))
	}
	return ids
}

// outboxRow is the part of the allocation the drain is responsible for.
type outboxRow struct {
	pending      bool
	attempts     int16
	nextAtSet    bool
	nextAtFuture bool
}

func getOutbox(t *testing.T, db *pgxpool.Pool, allocationID string) outboxRow {
	t.Helper()
	var r outboxRow
	err := db.QueryRow(t.Context(), `
		SELECT callback_pending,
		       callback_attempts,
		       callback_next_at IS NOT NULL,
		       callback_next_at IS NOT NULL AND callback_next_at > now()
		FROM allocation
		WHERE allocation_id = $1::text::uuid`, allocationID).
		Scan(&r.pending, &r.attempts, &r.nextAtSet, &r.nextAtFuture)
	if err != nil {
		t.Fatalf("read outbox row %q: %v", allocationID, err)
	}
	return r
}

// TestDrainDeliversAndClearsCallback checks the happy path end to end: the right path,
// bearer header, content type and body reach Session, and the outbox flag is cleared.
func TestDrainDeliversAndClearsCallback(t *testing.T) {
	db := openDB(t)
	f := newFakeSession(t, http.StatusNoContent)
	id := insertEnded(t, db, "server_dead")

	sent, failed, err := newNotifier(db, f).Drain(t.Context())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if sent != 1 || failed != 0 {
		t.Fatalf("Drain = (sent %d, failed %d), want (1, 0)", sent, failed)
	}

	reqs := f.all()
	if len(reqs) != 1 {
		t.Fatalf("Session saw %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if want := "/internal/session/allocations/" + id + "/ended"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if want := "Bearer " + callbackKey; got.authorization != want {
		t.Errorf("Authorization = %q, want %q", got.authorization, want)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.contentType)
	}

	var body struct {
		PartyID string `json:"party_id"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, got.body)
	}
	if body.PartyID != callbackParty {
		t.Errorf("party_id = %q, want %q", body.PartyID, callbackParty)
	}
	if body.Reason != "server_dead" {
		t.Errorf("reason = %q, want server_dead", body.Reason)
	}

	row := getOutbox(t, db, id)
	if row.pending {
		t.Error("callback_pending is still true after a 2xx")
	}
	if row.nextAtSet {
		t.Error("callback_next_at is set after delivery")
	}
}

// TestDrainRetriesOnServerError checks that a 500 is recorded: the attempt count rises,
// the next attempt is scheduled in the future, and the row stays in the outbox.
func TestDrainRetriesOnServerError(t *testing.T) {
	db := openDB(t)
	f := newFakeSession(t, http.StatusInternalServerError)
	id := insertEnded(t, db, "server_restarted")

	sent, failed, err := newNotifier(db, f).Drain(t.Context())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if sent != 0 || failed != 1 {
		t.Fatalf("Drain = (sent %d, failed %d), want (0, 1)", sent, failed)
	}

	row := getOutbox(t, db, id)
	if !row.pending {
		t.Error("callback_pending = false after one failure, want still pending")
	}
	if row.attempts != 1 {
		t.Errorf("callback_attempts = %d, want 1", row.attempts)
	}
	if !row.nextAtSet || !row.nextAtFuture {
		t.Errorf("callback_next_at = (set %v, future %v), want a future retry", row.nextAtSet, row.nextAtFuture)
	}
}

// TestDrainGivesUpAfterFiveFailures checks the retry budget: each failure re-arms the
// row, and the fifth clears the flag so Session's repair poll becomes the recovery path.
func TestDrainGivesUpAfterFiveFailures(t *testing.T) {
	db := openDB(t)
	f := newFakeSession(t, http.StatusInternalServerError)
	id := insertEnded(t, db, "ended")
	n := newNotifier(db, f)

	for i := 1; i <= 5; i++ {
		// The backoff leaves next_at in the future; pull it back so the next attempt
		// is due immediately rather than waiting out the schedule.
		if _, err := db.Exec(t.Context(), `
			UPDATE allocation SET callback_next_at = now() - interval '1 second'
			WHERE allocation_id = $1::text::uuid`, id); err != nil {
			t.Fatalf("make attempt %d due: %v", i, err)
		}
		sent, failed, err := n.Drain(t.Context())
		if err != nil {
			t.Fatalf("attempt %d: Drain: %v", i, err)
		}
		if sent != 0 || failed != 1 {
			t.Fatalf("attempt %d: Drain = (sent %d, failed %d), want (0, 1)", i, sent, failed)
		}
	}

	if got := f.count(); got != 5 {
		t.Errorf("Session saw %d requests, want 5", got)
	}
	row := getOutbox(t, db, id)
	if row.pending {
		t.Error("callback_pending = true after five failures, want false")
	}
	if row.attempts != 5 {
		t.Errorf("callback_attempts = %d, want 5", row.attempts)
	}
}

// TestDrainSkipsFutureCallback checks the due filter: a row whose backoff has not
// elapsed is not claimed and not delivered.
func TestDrainSkipsFutureCallback(t *testing.T) {
	db := openDB(t)
	f := newFakeSession(t, http.StatusNoContent)
	id := insertEnded(t, db, "ended")
	if _, err := db.Exec(t.Context(), `
		UPDATE allocation SET callback_next_at = now() + interval '1 minute'
		WHERE allocation_id = $1::text::uuid`, id); err != nil {
		t.Fatalf("schedule callback: %v", err)
	}

	sent, failed, err := newNotifier(db, f).Drain(t.Context())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if sent != 0 || failed != 0 {
		t.Fatalf("Drain = (sent %d, failed %d), want (0, 0)", sent, failed)
	}
	if got := f.count(); got != 0 {
		t.Errorf("Session saw %d requests for a not-yet-due row, want 0", got)
	}
	if row := getOutbox(t, db, id); !row.pending {
		t.Error("callback_pending = false, want still pending")
	}
}

// TestDrainConcurrentDeliversEachRowOnce checks the claim's guarantee: two drains racing
// over one backlog partition it, so every row is delivered exactly once. The fake
// Session counts requests, which catches both a row sent twice and a row never sent.
func TestDrainConcurrentDeliversEachRowOnce(t *testing.T) {
	db := openDB(t)
	f := newFakeSession(t, http.StatusNoContent)
	ids := insertEndedMany(t, db, 20)
	n := newNotifier(db, f)

	type result struct {
		sent   int
		failed int
		err    error
	}
	results := make([]result, 2)

	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sent, failed, err := n.Drain(t.Context())
			results[i] = result{sent: sent, failed: failed, err: err}
		}(i)
	}
	wg.Wait()

	totalSent := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("drain %d: %v", i, r.err)
		}
		if r.failed != 0 {
			t.Errorf("drain %d failed %d deliveries, want 0", i, r.failed)
		}
		totalSent += r.sent
	}
	if totalSent != len(ids) {
		t.Errorf("the two drains sent %d callbacks, want %d", totalSent, len(ids))
	}

	seen := make(map[string]int, len(ids))
	for _, req := range f.all() {
		id := strings.TrimSuffix(strings.TrimPrefix(req.path, "/internal/session/allocations/"), "/ended")
		seen[id]++
	}
	if len(seen) != len(ids) {
		t.Errorf("Session saw %d distinct allocations, want %d", len(seen), len(ids))
	}
	for _, id := range ids {
		switch seen[id] {
		case 1:
		case 0:
			t.Errorf("allocation %q was never delivered", id)
		default:
			t.Errorf("allocation %q was delivered %d times, want once", id, seen[id])
		}
	}
}
