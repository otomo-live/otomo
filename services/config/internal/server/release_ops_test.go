package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// lockStagingHead serialises the promotion tests across packages against the shared
// channel_head row, the same way lockDevHead does for dev.
func lockStagingHead(t *testing.T, db *store.DB) {
	t.Helper()

	ctx := context.Background()
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a lock connection: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1)::bigint)`, "config_test:staging"); err != nil {
		conn.Release()
		t.Fatalf("advisory lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1)::bigint)`, "config_test:staging")
		conn.Release()
	})
}

func stagingHead(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var id int64
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT release_id FROM channel_head WHERE channel = 'staging'`).Scan(&id); err != nil {
		t.Fatalf("read staging head: %v", err)
	}
	return id
}

func restoreStagingHead(t *testing.T, db *store.DB, id int64) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(),
		`UPDATE channel_head SET release_id = $1, updated_by = 'test-cleanup', updated_at = now()
		  WHERE channel = 'staging'`, id); err != nil {
		t.Fatalf("restore staging head: %v", err)
	}
}

func countStagingReleases(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM release WHERE channel = 'staging'`).Scan(&n); err != nil {
		t.Fatalf("count staging releases: %v", err)
	}
	return n
}

// TestReleaseOpsThroughTheLiveServer drives history, rollback, promote and the audit
// feed over a real listener, so the route table, the handlers, the store transaction and
// the audit writer are exercised together as a client sees them.
func TestReleaseOpsThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)
	lockDevHead(t, db)
	lockStagingHead(t, db)
	devOrig := devHead(t, db)
	stagingOrig := stagingHead(t, db)
	t.Cleanup(func() {
		restoreDevHead(t, db, devOrig)
		restoreStagingHead(t, db, stagingOrig)
	})

	ctx := context.Background()
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

	actor := store.Entry{ActorID: "staff-1", ActorName: "Release Operator"}
	publish := func(base int64, prefix, msg string) store.Release {
		t.Helper()
		_, version, _ := publishFixture(t, db, blobs, prefix)
		rel, err := db.Publish(ctx, "dev", store.PublishRequest{
			BaseReleaseID:    base,
			Versions:         []store.PublishVersion{{Namespace: prefix, Version: version}},
			MinClientVersion: "1.0.0",
			Message:          msg,
		}, actor, blobs)
		if err != nil {
			t.Fatalf("Publish %s: %v", prefix, err)
		}
		return rel
	}

	nsA := fmt.Sprintf("srv.ops_a_%d", time.Now().UnixNano())
	nsB := fmt.Sprintf("srv.ops_b_%d", time.Now().UnixNano())
	relA := publish(devOrig, nsA, "A")
	relB := publish(relA.ReleaseID, nsB, "B")

	viewer := h.token(t, []string{"viewer"}, nil)
	liveOps := h.token(t, []string{"live_ops"}, nil)
	admin := h.token(t, []string{"admin"}, nil)

	// History: newest first, head flagged, head id stated once, and an opaque cursor for
	// the next page.
	type historyRelease struct {
		ReleaseID            int64   `json:"release_id"`
		IsHead               bool    `json:"is_head"`
		ManifestSHA256       string  `json:"manifest_sha256"`
		ServerManifestSHA256 *string `json:"server_manifest_sha256"`
		Message              string  `json:"message"`
	}
	type historyBody struct {
		HeadReleaseID int64            `json:"head_release_id"`
		Releases      []historyRelease `json:"releases"`
		NextBefore    *int64           `json:"next_before"`
	}
	resp := h.do(t, h.public, http.MethodGet, "/api/admin/config/channels/dev/releases?limit=2", viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("history = %d, want 200 (%s)", resp.status, resp.body)
	}
	var hist historyBody
	if err := json.Unmarshal([]byte(resp.body), &hist); err != nil {
		t.Fatalf("history body: %v (%s)", err, resp.body)
	}
	if hist.HeadReleaseID != relB.ReleaseID {
		t.Errorf("head_release_id = %d, want %d", hist.HeadReleaseID, relB.ReleaseID)
	}
	if len(hist.Releases) != 2 || hist.Releases[0].ReleaseID != relB.ReleaseID || hist.Releases[1].ReleaseID != relA.ReleaseID {
		t.Fatalf("history releases = %+v, want %d then %d", hist.Releases, relB.ReleaseID, relA.ReleaseID)
	}
	if !hist.Releases[0].IsHead || hist.Releases[1].IsHead {
		t.Errorf("is_head = %v,%v, want true,false", hist.Releases[0].IsHead, hist.Releases[1].IsHead)
	}
	// The history lists each release's server manifest hash; these releases have one.
	if hist.Releases[0].ServerManifestSHA256 == nil || *hist.Releases[0].ServerManifestSHA256 == "" {
		t.Errorf("history server_manifest_sha256 = %v, want a non-empty string", hist.Releases[0].ServerManifestSHA256)
	}
	if hist.Releases[1].ServerManifestSHA256 == nil || *hist.Releases[1].ServerManifestSHA256 == "" {
		t.Errorf("older history server_manifest_sha256 = %v, want a non-empty string", hist.Releases[1].ServerManifestSHA256)
	}
	if hist.NextBefore == nil {
		t.Fatal("history first page has no next_before")
	}

	if resp := h.do(t, h.public, http.MethodGet, "/api/admin/config/channels/dev/releases?before=x", viewer); resp.status != http.StatusBadRequest {
		t.Errorf("bad before = %d, want 400", resp.status)
	}
	if resp := h.do(t, h.public, http.MethodGet, "/api/admin/config/channels/prod/releases", viewer); resp.status != http.StatusBadRequest {
		t.Errorf("unknown channel = %d, want 400", resp.status)
	}

	// live_ops may not roll back, at the route table as well as in the handler.
	rollbackBody := fmt.Sprintf(`{"release_id":%d,"base_release_id":%d}`, relA.ReleaseID, relB.ReleaseID)
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/rollback", liveOps, rollbackBody); resp.status != http.StatusForbidden {
		t.Fatalf("live_ops rollback = %d, want 403 (%s)", resp.status, resp.body)
	}

	releaseBefore := countDevReleases(t, db)
	resp = h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/rollback", admin, rollbackBody)
	if resp.status != http.StatusOK {
		t.Fatalf("rollback = %d, want 200 (%s)", resp.status, resp.body)
	}
	var rolled struct {
		ReleaseID int64           `json:"release_id"`
		Channel   string          `json:"channel"`
		Manifest  json.RawMessage `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(resp.body), &rolled); err != nil {
		t.Fatalf("rollback body: %v (%s)", err, resp.body)
	}
	if rolled.ReleaseID != relA.ReleaseID || len(rolled.Manifest) == 0 {
		t.Errorf("rollback response = %+v, want release %d with a manifest", rolled, relA.ReleaseID)
	}
	if got := devHead(t, db); got != relA.ReleaseID {
		t.Errorf("dev head = %d, want %d", got, relA.ReleaseID)
	}
	if got := countDevReleases(t, db); got != releaseBefore {
		t.Errorf("dev release count = %d after rollback, want %d", got, releaseBefore)
	}

	// Two more successful rollbacks leave the head back at A and give the audit cursor
	// more than one page to walk.
	for _, step := range []struct{ to, base int64 }{
		{relB.ReleaseID, relA.ReleaseID},
		{relA.ReleaseID, relB.ReleaseID},
	} {
		body := fmt.Sprintf(`{"release_id":%d,"base_release_id":%d}`, step.to, step.base)
		if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/rollback", admin, body); resp.status != http.StatusOK {
			t.Fatalf("rollback to %d = %d, want 200 (%s)", step.to, resp.status, resp.body)
		}
	}
	if got := devHead(t, db); got != relA.ReleaseID {
		t.Fatalf("dev head = %d, want %d", got, relA.ReleaseID)
	}

	// A staging release is not a dev release: 404 naming both.
	stagingBody := fmt.Sprintf(`{"release_id":%d,"base_release_id":%d}`, stagingOrig, relA.ReleaseID)
	resp = h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/rollback", admin, stagingBody)
	if resp.status != http.StatusNotFound {
		t.Fatalf("rollback to staging release = %d, want 404 (%s)", resp.status, resp.body)
	}
	if !strings.Contains(resp.body, fmt.Sprintf("release %d is not a release of channel dev", stagingOrig)) {
		t.Errorf("404 message = %s", resp.body)
	}

	// Rolling back onto the head, and a stale base, are both conflicts.
	noChangeBody := fmt.Sprintf(`{"release_id":%d,"base_release_id":%d}`, relA.ReleaseID, relA.ReleaseID)
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/rollback", admin, noChangeBody); resp.status != http.StatusConflict || resp.code(t) != "no_changes" {
		t.Errorf("rollback to head = %d/%s, want 409/no_changes", resp.status, resp.body)
	}
	staleBody := fmt.Sprintf(`{"release_id":%d,"base_release_id":%d}`, relA.ReleaseID, devOrig)
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/rollback", admin, staleBody); resp.status != http.StatusConflict || resp.code(t) != "stale_release" {
		t.Errorf("stale rollback = %d/%s, want 409/stale_release", resp.status, resp.body)
	}

	// Promote dev's content onto staging.
	promoteBody := fmt.Sprintf(`{"base_release_id":%d}`, stagingOrig)
	stagingBefore := countStagingReleases(t, db)
	resp = h.doJSON(t, http.MethodPost, "/api/admin/config/channels/staging/promote?from=dev", admin, promoteBody)
	if resp.status != http.StatusCreated {
		t.Fatalf("promote = %d, want 201 (%s)", resp.status, resp.body)
	}
	var promoted struct {
		ReleaseID int64           `json:"release_id"`
		Channel   string          `json:"channel"`
		Manifest  json.RawMessage `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(resp.body), &promoted); err != nil {
		t.Fatalf("promote body: %v (%s)", err, resp.body)
	}
	if promoted.Channel != "staging" {
		t.Errorf("promoted channel = %q, want staging", promoted.Channel)
	}
	if got := stagingHead(t, db); got != promoted.ReleaseID {
		t.Errorf("staging head = %d, want %d", got, promoted.ReleaseID)
	}
	if got := countStagingReleases(t, db); got != stagingBefore+1 {
		t.Errorf("staging release count = %d, want %d", got, stagingBefore+1)
	}
	var promotedManifest struct {
		Channel   string `json:"channel"`
		ReleaseID int64  `json:"release_id"`
	}
	if err := json.Unmarshal(promoted.Manifest, &promotedManifest); err != nil {
		t.Fatalf("promoted manifest: %v", err)
	}
	if promotedManifest.Channel != "staging" || promotedManifest.ReleaseID != promoted.ReleaseID {
		t.Errorf("promoted manifest header = %+v", promotedManifest)
	}

	// Promoting the same content again is a no-op conflict.
	againBody := fmt.Sprintf(`{"base_release_id":%d}`, promoted.ReleaseID)
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/staging/promote?from=dev", admin, againBody); resp.status != http.StatusConflict || resp.code(t) != "no_changes" {
		t.Errorf("re-promote = %d/%s, want 409/no_changes", resp.status, resp.body)
	}
	// live may only promote from staging.
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/live/promote?from=dev", admin, promoteBody); resp.status != http.StatusBadRequest {
		t.Errorf("live?from=dev = %d, want 400 (%s)", resp.status, resp.body)
	}
	// live_ops may not promote at all.
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/staging/promote?from=dev", liveOps, promoteBody); resp.status != http.StatusForbidden {
		t.Errorf("live_ops promote = %d, want 403", resp.status)
	}

	// The audit feed carries the rollback and the promotion, with the exact entry shape
	// the Dashboard merges.
	type auditEntry struct {
		ID        int64           `json:"id"`
		At        time.Time       `json:"at"`
		ActorID   string          `json:"actor_id"`
		ActorName string          `json:"actor_name"`
		Source    string          `json:"source"`
		Action    string          `json:"action"`
		Target    string          `json:"target"`
		Details   json.RawMessage `json:"details"`
	}
	type auditBody struct {
		Entries    []auditEntry `json:"entries"`
		NextCursor *string      `json:"next_cursor"`
	}
	auditPath := "/api/admin/config/audit?action=release.rollback&actor=staff-1&limit=1"
	resp = h.do(t, h.public, http.MethodGet, auditPath, viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("audit = %d, want 200 (%s)", resp.status, resp.body)
	}
	var feed auditBody
	if err := json.Unmarshal([]byte(resp.body), &feed); err != nil {
		t.Fatalf("audit body: %v (%s)", err, resp.body)
	}
	if len(feed.Entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(feed.Entries))
	}
	first := feed.Entries[0]
	if first.Action != "release.rollback" || first.ActorID != "staff-1" || first.Target != "dev" || first.Source != "config" {
		t.Errorf("audit entry = %+v", first)
	}
	if first.At.IsZero() {
		t.Error("audit entry has no at")
	}
	if first.Details == nil || !json.Valid(first.Details) {
		t.Errorf("audit details = %s", first.Details)
	}
	if feed.NextCursor == nil {
		t.Fatal("audit first page has no next_cursor")
	}

	// The cursor pages strictly downward.
	resp = h.do(t, h.public, http.MethodGet,
		"/api/admin/config/audit?action=release.rollback&actor=staff-1&limit=1&cursor="+*feed.NextCursor, viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("audit page 2 = %d, want 200 (%s)", resp.status, resp.body)
	}
	var page2 auditBody
	if err := json.Unmarshal([]byte(resp.body), &page2); err != nil {
		t.Fatalf("audit page 2 body: %v", err)
	}
	if len(page2.Entries) != 1 || page2.Entries[0].ID >= first.ID {
		t.Errorf("audit page 2 = %+v, want an entry older than %d", page2.Entries, first.ID)
	}

	// A malformed cursor is a 400, not a silent restart.
	if resp := h.do(t, h.public, http.MethodGet, "/api/admin/config/audit?cursor=!!!", viewer); resp.status != http.StatusBadRequest {
		t.Errorf("malformed cursor = %d, want 400", resp.status)
	}
	if resp := h.do(t, h.public, http.MethodGet, "/api/admin/config/audit?from=yesterday", viewer); resp.status != http.StatusBadRequest {
		t.Errorf("bad from = %d, want 400", resp.status)
	}
}
