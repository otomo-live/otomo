package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// This file holds the concurrency matrix at the HTTP boundary. The store package proves
// the same serialisation directly; these tests prove the live listener returns the
// statuses and bodies a client can act on while the race is happening, including the
// one pg_notify a successful publish must produce.

// startGate returns a WaitGroup the callers mark done once they are ready, a channel the
// test closes to release them all at once, and a WaitGroup the test waits on so it does
// not read a caller's result before the caller has written it.
func startGate(n int) (*sync.WaitGroup, chan struct{}, *sync.WaitGroup) {
	var ready, done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	return &ready, make(chan struct{}), &done
}

// httpCall is one raw request/response without a *testing.T, so it can be produced from
// a racing goroutine; the test asserts on the collected calls afterwards.
type httpCall struct {
	status int
	body   string
	err    error
}

// rawJSON issues one JSON request and reads the whole response. It is the goroutine-safe
// counterpart of harness.doJSON, which calls t.Fatalf and therefore may not run off the
// test goroutine.
func rawJSON(ctx context.Context, h *harness, method, path, token, body string) httpCall {
	return rawJSONHeaders(ctx, h, method, path, token, body, nil)
}

// rawJSONHeaders is rawJSON plus extra request headers, for the schema PUT's If-Match
// precondition.
func rawJSONHeaders(ctx context.Context, h *harness, method, path, token, body string, headers map[string]string) httpCall {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+h.public+path, strings.NewReader(body))
	if err != nil {
		return httpCall{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return httpCall{err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return httpCall{err: err}
	}
	return httpCall{status: resp.StatusCode, body: string(raw)}
}

func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want is not JSON: %v (%s)", err, want)
	}
	return reflect.DeepEqual(g, w)
}

// TestConcurrentDraftSavesThroughTheLiveServer is requirement 1 over HTTP: eight live_ops
// saves at the same revision must yield one 200, seven 409 stale_revision, and a draft
// holding exactly the winner's document.
func TestConcurrentDraftSavesThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)
	h := newHarness(t, nil, &api.Handlers{
		Store: db,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	name := fmt.Sprintf("srv.conc_draft_%d", time.Now().UnixNano())
	actor := store.Entry{ActorID: "staff-1", ActorName: "Draft Operator"}
	if _, err := db.CreateNamespace(t.Context(), name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	base := "/api/admin/config/namespaces/" + name + "/draft"
	liveOps := h.token(t, []string{"live_ops"}, nil)
	viewer := h.token(t, []string{"viewer"}, nil)

	const writers = 8
	docs := make([]string, writers)
	bodies := make([]string, writers)
	for i := 0; i < writers; i++ {
		docs[i] = fmt.Sprintf(`{"winner":%d}`, i)
		bodies[i] = fmt.Sprintf(`{"document":%s,"revision":1}`, docs[i])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ready, start, done := startGate(writers)
	calls := make([]httpCall, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			ready.Done()
			<-start
			defer done.Done()
			calls[i] = rawJSON(ctx, h, http.MethodPut, base, liveOps, bodies[i])
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	var wins, conflicts int
	winner := -1
	for i, c := range calls {
		if c.err != nil {
			t.Fatalf("save %d: %v", i, c.err)
		}
		switch c.status {
		case http.StatusOK:
			wins++
			winner = i
		case http.StatusConflict:
			conflicts++
			if got := (response{status: c.status, body: c.body}).code(t); got != "stale_revision" {
				t.Errorf("save %d conflict code = %q, want stale_revision", i, got)
			}
		default:
			t.Errorf("save %d status = %d, want 200 or 409 (%s)", i, c.status, c.body)
		}
	}
	if wins != 1 || conflicts != writers-1 {
		t.Fatalf("outcomes = %d ok / %d conflict, want 1 / %d", wins, conflicts, writers-1)
	}

	resp := h.do(t, h.public, http.MethodGet, base, viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("GET draft = %d (%s)", resp.status, resp.body)
	}
	var got struct {
		Document json.RawMessage `json:"document"`
		Revision int             `json:"revision"`
	}
	if err := json.Unmarshal([]byte(resp.body), &got); err != nil {
		t.Fatalf("GET draft body is not JSON: %v (%s)", err, resp.body)
	}
	if got.Revision != 2 {
		t.Errorf("final revision = %d, want 2", got.Revision)
	}
	if !jsonEqual(t, got.Document, []byte(docs[winner])) {
		t.Errorf("surviving document = %s, want the winner's %s", got.Document, docs[winner])
	}

	var audits int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'draft.save' AND target = $1`, name).Scan(&audits); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("%d draft.save audit rows, want 1", audits)
	}
}

// TestConcurrentPublishesThroughTheLiveServer is requirement 3: eight publishes with the
// same base must produce one 201 and seven 409 stale_release, move the head once, write
// one release and one audit row, and deliver exactly one pg_notify.
func TestConcurrentPublishesThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)
	lockDevHead(t, db)
	original := devHead(t, db)
	t.Cleanup(func() { restoreDevHead(t, db, original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	h := newHarness(t, nil, &api.Handlers{
		Store:   db,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Schemas: &schema.Cache{},
		Blobs:   blobs,
	})

	name := fmt.Sprintf("srv.conc_publish_%d", time.Now().UnixNano())
	_, version, _ := publishFixture(t, db, blobs, name)
	before := countDevReleases(t, db)

	// LISTEN before the race, on a connection the pool keeps out of the handler's way.
	ctx := context.Background()
	listener, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listener: %v", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, `LISTEN config_release`); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	body := fmt.Sprintf(
		`{"base_release_id":%d,"versions":[{"namespace":%q,"version":%d}],"packs":[],"min_client_version":"1.4.0","message":"race"}`,
		original, name, version)
	liveOps := h.token(t, []string{"live_ops"}, nil)
	path := "/api/admin/config/channels/dev/releases"

	const writers = 8
	raceCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ready, start, done := startGate(writers)
	calls := make([]httpCall, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			ready.Done()
			<-start
			defer done.Done()
			calls[i] = rawJSON(raceCtx, h, http.MethodPost, path, liveOps, body)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	var created, stale int
	winner := -1
	for i, c := range calls {
		if c.err != nil {
			t.Fatalf("publish %d: %v", i, c.err)
		}
		switch c.status {
		case http.StatusCreated:
			created++
			winner = i
		case http.StatusConflict:
			stale++
			if got := (response{status: c.status, body: c.body}).code(t); got != "stale_release" {
				t.Errorf("publish %d conflict code = %q, want stale_release", i, got)
			}
		default:
			t.Errorf("publish %d status = %d, want 201 or 409 (%s)", i, c.status, c.body)
		}
	}
	if created != 1 || stale != writers-1 {
		t.Fatalf("outcomes = %d created / %d conflict, want 1 / %d", created, stale, writers-1)
	}

	var rel struct {
		ReleaseID int64 `json:"release_id"`
	}
	if err := json.Unmarshal([]byte(calls[winner].body), &rel); err != nil {
		t.Fatalf("201 body is not JSON: %v (%s)", err, calls[winner].body)
	}
	if got := devHead(t, db); got != rel.ReleaseID {
		t.Errorf("dev head = %d, want the winner %d", got, rel.ReleaseID)
	}
	if rel.ReleaseID == original {
		t.Error("the winning release id equals the old head; the head did not move")
	}
	if got := countDevReleases(t, db); got != before+1 {
		t.Errorf("dev release count = %d, want %d", got, before+1)
	}
	var audits int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log
		  WHERE action = 'release.publish' AND target = 'dev'
		    AND (details->>'release_id')::bigint = $1`, rel.ReleaseID).Scan(&audits); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("%d release.publish audit rows for release %d, want 1", audits, rel.ReleaseID)
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Second)
	defer waitCancel()
	notification, err := listener.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if notification.Payload != "dev" {
		t.Errorf("notification payload = %q, want dev", notification.Payload)
	}
	secondCtx, secondCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer secondCancel()
	if _, err := listener.Conn().WaitForNotification(secondCtx); err == nil {
		t.Error("a second notification arrived for one successful publish")
	}
}

// TestConcurrentSchemaReplacesThroughTheLiveServer is requirement 5 at the HTTP
// boundary: eight admin PUTs all preconditioned on v1 must yield exactly one 200 at v2
// and seven 409 stale_schema, with exactly one audit row. The store's
// TestReplaceSchemaIsSerialised covers the other half of the contract — gap-free
// versions under a client that retries with the version it re-read.
func TestConcurrentSchemaReplacesThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)
	h := newHarness(t, nil, &api.Handlers{
		Store: db,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	name := fmt.Sprintf("srv.conc_schema_%d", time.Now().UnixNano())
	actor := store.Entry{ActorID: "staff-1", ActorName: "Schema Admin"}
	if _, err := db.CreateNamespace(t.Context(), name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	base := "/api/admin/config/namespaces/" + name + "/schema"
	admin := h.token(t, []string{"admin"}, nil)

	const writers = 8
	bodies := make([]string, writers)
	for i := 0; i < writers; i++ {
		bodies[i] = fmt.Sprintf(`{"type":"object","title":"s%d"}`, i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ready, start, done := startGate(writers)
	calls := make([]httpCall, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			ready.Done()
			<-start
			defer done.Done()
			calls[i] = rawJSONHeaders(ctx, h, http.MethodPut, base, admin, bodies[i],
				map[string]string{"If-Match": `"1"`})
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	winners := 0
	for i, c := range calls {
		if c.err != nil {
			t.Fatalf("replace %d: %v", i, c.err)
		}
		switch c.status {
		case http.StatusOK:
			winners++
			var got struct {
				SchemaVersion int `json:"schema_version"`
			}
			if err := json.Unmarshal([]byte(c.body), &got); err != nil {
				t.Fatalf("winner body is not JSON: %v (%s)", err, c.body)
			}
			if got.SchemaVersion != 2 {
				t.Errorf("winner wrote schema version %d, want 2", got.SchemaVersion)
			}
		case http.StatusConflict:
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(c.body), &env); err != nil {
				t.Fatalf("loser body is not JSON: %v (%s)", err, c.body)
			}
			if env.Error.Code != "stale_schema" {
				t.Errorf("loser %d code = %q, want stale_schema", i, env.Error.Code)
			}
		default:
			t.Fatalf("replace %d status = %d, want 200 or 409 (%s)", i, c.status, c.body)
		}
	}
	if winners != 1 {
		t.Errorf("%d of %d PUTs succeeded, want exactly 1", winners, writers)
	}

	rows, err := db.Pool.Query(ctx,
		`SELECT schema_version FROM config_schema WHERE namespace = $1 ORDER BY schema_version`, name)
	if err != nil {
		t.Fatalf("read schema versions: %v", err)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		versions = append(versions, v)
	}
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("stored schema versions = %v, want [1 2]", versions)
	}

	var audits int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'schema.replace' AND target = $1`, name).Scan(&audits); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("%d schema.replace audit rows, want exactly 1", audits)
	}
}

// installAuditBlockerServer is the server package's copy of the store test helper: a
// trigger that makes any audit insert carrying target fail, dropped when the test ends.
func installAuditBlockerServer(t *testing.T, db *store.DB, target string) {
	t.Helper()

	id := fmt.Sprintf("%d_%d", time.Now().UnixNano(), auditBlockerSeq.Add(1))
	fn := "test_srv_audit_block_fn_" + id
	trg := "test_srv_audit_block_trg_" + id
	ctx := context.Background()

	if _, err := db.Pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger AS $block$
		BEGIN
		    IF NEW.target = '%s' THEN
		        RAISE EXCEPTION 'audit write blocked by test';
		    END IF;
		    RETURN NEW;
		END
		$block$ LANGUAGE plpgsql`, fn, strings.ReplaceAll(target, "'", "''"))); err != nil {
		t.Fatalf("create audit blocker function: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION %s()`, trg, fn)); err != nil {
		t.Fatalf("create audit blocker trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON audit_log`, trg))
		_, _ = db.Pool.Exec(context.Background(),
			fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
}

// auditBlockerSeq disambiguates the DDL names when two blockers are installed in the same
// clock tick.
var auditBlockerSeq atomic.Int64

// TestAuditFailureIsA500ThroughTheLiveServer closes the loop on requirement 6 at the
// boundary: an audit insert the database refuses is not a client error, so the handler
// answers 500 and the draft is left exactly as it was.
func TestAuditFailureIsA500ThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)
	h := newHarness(t, nil, &api.Handlers{
		Store: db,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	name := fmt.Sprintf("srv.audit_fail_%d", time.Now().UnixNano())
	actor := store.Entry{ActorID: "staff-1", ActorName: "Draft Operator"}
	if _, err := db.CreateNamespace(t.Context(), name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	installAuditBlockerServer(t, db, name)

	base := "/api/admin/config/namespaces/" + name + "/draft"
	liveOps := h.token(t, []string{"live_ops"}, nil)
	resp := h.doJSON(t, http.MethodPut, base, liveOps, `{"document":{"level":9},"revision":1}`)
	if resp.status != http.StatusInternalServerError {
		t.Fatalf("blocked save = %d, want 500 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "internal_error" {
		t.Errorf("blocked save code = %q, want internal_error", code)
	}

	d, err := db.Draft(context.Background(), name)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if d.Revision != 1 || string(d.Body) != "{}" {
		t.Errorf("draft after the blocked save = r%d %s, want r1 {}", d.Revision, d.Body)
	}
}
