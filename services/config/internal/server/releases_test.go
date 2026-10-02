package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// lockDevHead serialises publishing tests across packages, which matters because
// channel_head is shared global state: Go runs different packages' test binaries
// concurrently, so one package's publish must not move the head out from under another's
// preview. The session advisory lock is released by the cleanup.
func lockDevHead(t *testing.T, db *store.DB) {
	t.Helper()

	ctx := context.Background()
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a lock connection: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1)::bigint)`, "config_test:dev"); err != nil {
		conn.Release()
		t.Fatalf("advisory lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1)::bigint)`, "config_test:dev")
		conn.Release()
	})
}

func devHead(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var id int64
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT release_id FROM channel_head WHERE channel = 'dev'`).Scan(&id); err != nil {
		t.Fatalf("read dev head: %v", err)
	}
	return id
}

func restoreDevHead(t *testing.T, db *store.DB, id int64) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(),
		`UPDATE channel_head SET release_id = $1, updated_by = 'test-cleanup', updated_at = now()
		  WHERE channel = 'dev'`, id); err != nil {
		t.Fatalf("restore dev head: %v", err)
	}
}

func countDevReleases(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM release WHERE channel = 'dev'`).Scan(&n); err != nil {
		t.Fatalf("count dev releases: %v", err)
	}
	return n
}

// publishFixture goes through the same store calls the other handlers do: a namespace,
// a schema, a draft, one immutable version and one uploaded pack.
func publishFixture(t *testing.T, db *store.DB, blobs *blob.Store, name string) (store.Entry, int, blob.Ref) {
	t.Helper()

	ctx := context.Background()
	actor := store.Entry{ActorID: "staff-1", ActorName: "Release Operator"}
	if _, err := db.CreateNamespace(ctx, name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if _, err := db.ReplaceSchema(ctx, name, []byte(`{"type":"object"}`), nil, actor); err != nil {
		t.Fatalf("ReplaceSchema: %v", err)
	}
	if _, err := db.SaveDraft(ctx, name, []byte(`{"level":1}`), 1, actor); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	v, err := db.CreateVersion(ctx, name, 2, "v1", actor, func(body []byte, _ int, _ []byte) ([]byte, string, error) {
		canonical, err := schema.Canonical(body)
		if err != nil {
			return nil, "", err
		}
		ref, err := blobs.PutBytes(ctx, canonical)
		if err != nil {
			return nil, "", err
		}
		return canonical, ref.SHA256, nil
	})
	if err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	packRef, err := blobs.PutBytes(ctx, []byte("GDPC-fake-pack-payload"))
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if _, _, err := db.CreatePack(ctx, fmt.Sprintf("pack_%d", time.Now().UnixNano()), packRef, actor); err != nil {
		t.Fatalf("CreatePack: %v", err)
	}
	return actor, v.Version, packRef
}

// TestReleasesRoutesThroughTheLiveServer is the whole publish path over a real
// listener: 201 plus a client manifest whose sizes come from disk, the exact Patch hash
// check, the head move, the audit row, and every client-error edge.
func TestReleasesRoutesThroughTheLiveServer(t *testing.T) {
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

	name := fmt.Sprintf("srv.publish_%d", time.Now().UnixNano())
	_, version, pack := publishFixture(t, db, blobs, name)
	liveOps := h.token(t, []string{"live_ops"}, nil)
	admin := h.token(t, []string{"admin"}, nil)

	body := fmt.Sprintf(
		`{"base_release_id":%d,"versions":[{"namespace":%q,"version":%d}],"packs":[{"sha256":%q}],"min_client_version":"1.4.0","message":"nerf the sword"}`,
		original, name, version, pack.SHA256)

	resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/releases", liveOps, body)
	if resp.status != http.StatusCreated {
		t.Fatalf("publish = %d, want 201 (%s)", resp.status, resp.body)
	}
	var rel struct {
		ReleaseID            int64           `json:"release_id"`
		Channel              string          `json:"channel"`
		ManifestSHA256       string          `json:"manifest_sha256"`
		MinClientVersion     string          `json:"min_client_version"`
		Message              string          `json:"message"`
		CreatedBy            string          `json:"created_by"`
		CreatedAt            time.Time       `json:"created_at"`
		Manifest             json.RawMessage `json:"manifest"`
		ServerManifestSHA256 *string         `json:"server_manifest_sha256"`
		ServerManifest       json.RawMessage `json:"server_manifest"`
	}
	if err := json.Unmarshal([]byte(resp.body), &rel); err != nil {
		t.Fatalf("created body is not JSON: %v (%s)", err, resp.body)
	}
	if rel.Channel != "dev" || rel.MinClientVersion != "1.4.0" || rel.Message != "nerf the sword" {
		t.Errorf("release = %+v", rel)
	}
	if rel.CreatedBy != "staff-1" || rel.CreatedAt.IsZero() {
		t.Errorf("release attribution = %q/%v", rel.CreatedBy, rel.CreatedAt)
	}
	if rel.ReleaseID <= original {
		t.Errorf("release_id = %d, want greater than old head %d", rel.ReleaseID, original)
	}
	// Every release now carries a server manifest, even with no server namespaces.
	if rel.ServerManifestSHA256 == nil || *rel.ServerManifestSHA256 == "" {
		t.Errorf("server_manifest_sha256 = %v, want a non-empty string", rel.ServerManifestSHA256)
	}
	if len(rel.ServerManifest) == 0 {
		t.Error("server_manifest is empty, want the canonical document")
	}
	serverSHA := sha256.Sum256(rel.ServerManifest)
	if hex.EncodeToString(serverSHA[:]) != *rel.ServerManifestSHA256 {
		t.Errorf("server_manifest_sha256 = %s, want the hash of server_manifest", *rel.ServerManifestSHA256)
	}

	var manifest struct {
		Config map[string]struct {
			Version int    `json:"version"`
			SHA256  string `json:"sha256"`
			Size    int64  `json:"size"`
		} `json:"config"`
		Packs []struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"packs"`
	}
	if err := json.Unmarshal(rel.Manifest, &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	cfg, ok := manifest.Config[name]
	if !ok || cfg.Version != version || cfg.Size <= 0 {
		t.Errorf("manifest config[%s] = %+v", name, cfg)
	}
	if len(manifest.Packs) != 1 || manifest.Packs[0].SHA256 != pack.SHA256 || manifest.Packs[0].Size != pack.Size {
		t.Errorf("manifest packs = %+v, want %s size %d", manifest.Packs, pack.SHA256, pack.Size)
	}

	// The exact check Patch performs against manifest_sha256.
	var (
		storedText string
		storedSHA  string
	)
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT manifest::text, manifest_sha256 FROM release WHERE release_id = $1`, rel.ReleaseID).Scan(&storedText, &storedSHA); err != nil {
		t.Fatalf("read stored manifest: %v", err)
	}
	canonical, err := schema.Canonical([]byte(storedText))
	if err != nil {
		t.Fatalf("Canonical(stored): %v", err)
	}
	got := sha256.Sum256(canonical)
	if hex.EncodeToString(got[:]) != rel.ManifestSHA256 || storedSHA != rel.ManifestSHA256 {
		t.Errorf("manifest_sha256 = %s, canonical stored = %s, column = %s",
			rel.ManifestSHA256, hex.EncodeToString(got[:]), storedSHA)
	}

	if got := devHead(t, db); got != rel.ReleaseID {
		t.Errorf("dev head = %d, want %d", got, rel.ReleaseID)
	}
	var audits int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log
		  WHERE action = 'release.publish' AND target = 'dev'
		    AND (details->>'release_id')::bigint = $1`, rel.ReleaseID).Scan(&audits); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("%d release.publish audit rows for release %d, want 1", audits, rel.ReleaseID)
	}

	// A live_ops token must not reach the live literal route even with the real handler
	// wired; the guard answers before the body is read.
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/live/releases", liveOps, body); resp.status != http.StatusForbidden {
		t.Errorf("live_ops publish to live = %d, want 403 (%s)", resp.status, resp.body)
	}
	// An admin does reach it, and the channel comes from the literal path.
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/live/releases", admin, body); resp.status != http.StatusConflict {
		t.Errorf("admin publish to live with dev base = %d, want 409 (%s)", resp.status, resp.body)
	}

	// Stale base: 409, head unchanged, no new release.
	headAfterPublish := devHead(t, db)
	releasesAfterPublish := countDevReleases(t, db)
	resp = h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/releases", liveOps, body)
	if resp.status != http.StatusConflict {
		t.Fatalf("stale publish = %d, want 409 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "stale_release" {
		t.Errorf("stale code = %q, want stale_release", code)
	}
	if !strings.Contains(resp.body, "has moved from release") {
		t.Errorf("stale message does not describe the move: %s", resp.body)
	}
	if got := devHead(t, db); got != headAfterPublish {
		t.Errorf("dev head = %d after stale publish, want %d", got, headAfterPublish)
	}
	if got := countDevReleases(t, db); got != releasesAfterPublish {
		t.Errorf("dev release count = %d after stale publish, want %d", got, releasesAfterPublish)
	}

	// Server-audience namespace: accepted; it lands in the server manifest only.
	serverName := fmt.Sprintf("srv.serveronly_%d", time.Now().UnixNano())
	serverActor := store.Entry{ActorID: "staff-2", ActorName: "Server Operator"}
	if _, err := db.CreateNamespace(context.Background(), serverName, "server", "", serverActor); err != nil {
		t.Fatalf("CreateNamespace(server): %v", err)
	}
	if _, err := db.SaveDraft(context.Background(), serverName, []byte(`{"level":1}`), 1, serverActor); err != nil {
		t.Fatalf("SaveDraft(server): %v", err)
	}
	serverVersion, err := db.CreateVersion(context.Background(), serverName, 2, "v1", serverActor,
		func(b []byte, _ int, _ []byte) ([]byte, string, error) {
			canonical, err := schema.Canonical(b)
			if err != nil {
				return nil, "", err
			}
			ref, err := blobs.PutBytes(context.Background(), canonical)
			if err != nil {
				return nil, "", err
			}
			return canonical, ref.SHA256, nil
		})
	if err != nil {
		t.Fatalf("CreateVersion(server): %v", err)
	}
	serverBody := fmt.Sprintf(
		`{"base_release_id":%d,"versions":[{"namespace":%q,"version":%d}],"packs":[],"min_client_version":"1.4.0","message":"server split"}`,
		devHead(t, db), serverName, serverVersion.Version)
	resp = h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/releases", liveOps, serverBody)
	if resp.status != http.StatusCreated {
		t.Fatalf("server namespace publish = %d, want 201 (%s)", resp.status, resp.body)
	}
	var serverRel struct {
		Manifest             json.RawMessage `json:"manifest"`
		ServerManifest       json.RawMessage `json:"server_manifest"`
		ServerManifestSHA256 *string         `json:"server_manifest_sha256"`
	}
	if err := json.Unmarshal([]byte(resp.body), &serverRel); err != nil {
		t.Fatalf("server publish body: %v (%s)", err, resp.body)
	}
	var serverClient, serverServer struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(serverRel.Manifest, &serverClient); err != nil {
		t.Fatalf("client manifest: %v", err)
	}
	if len(serverClient.Config) != 0 {
		t.Errorf("client manifest config = %v, want empty for a server-only publish", serverClient.Config)
	}
	if err := json.Unmarshal(serverRel.ServerManifest, &serverServer); err != nil {
		t.Fatalf("server manifest: %v", err)
	}
	if _, ok := serverServer.Config[serverName]; !ok {
		t.Errorf("server manifest config = %v, want %s", serverServer.Config, serverName)
	}
	if serverRel.ServerManifestSHA256 == nil || *serverRel.ServerManifestSHA256 == "" {
		t.Errorf("server_manifest_sha256 = %v, want a non-empty string", serverRel.ServerManifestSHA256)
	}

	// Unknown version and unknown pack are 404s that name what was missing.
	unknownVersionBody := fmt.Sprintf(
		`{"base_release_id":%d,"versions":[{"namespace":%q,"version":999}],"packs":[],"min_client_version":"1.4.0","message":"x"}`,
		devHead(t, db), name)
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/releases", liveOps, unknownVersionBody); resp.status != http.StatusNotFound {
		t.Errorf("unknown version = %d, want 404 (%s)", resp.status, resp.body)
	}
	unknownPackBody := fmt.Sprintf(
		`{"base_release_id":%d,"versions":[],"packs":[{"sha256":%q}],"min_client_version":"1.4.0","message":"x"}`,
		devHead(t, db), strings.Repeat("b", 64))
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/releases", liveOps, unknownPackBody); resp.status != http.StatusNotFound {
		t.Errorf("unknown pack = %d, want 404 (%s)", resp.status, resp.body)
	}

	// A validation failure is a 400 and names the field.
	badBody := fmt.Sprintf(
		`{"base_release_id":%d,"versions":[],"packs":[],"min_client_version":"1.4","message":"x"}`,
		devHead(t, db))
	resp = h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/releases", liveOps, badBody)
	if resp.status != http.StatusBadRequest {
		t.Fatalf("bad min_client_version = %d, want 400 (%s)", resp.status, resp.body)
	}
	if !strings.Contains(resp.body, "min_client_version") {
		t.Errorf("validation message does not name min_client_version: %s", resp.body)
	}
}

// TestPublishConcurrentThroughTheLiveServer is the serialisation contract at the HTTP
// boundary: two publishes with the same base produce exactly one 201 and one 409.
func TestPublishConcurrentThroughTheLiveServer(t *testing.T) {
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

	name := fmt.Sprintf("srv.publish_race_%d", time.Now().UnixNano())
	_, version, _ := publishFixture(t, db, blobs, name)
	token := h.token(t, []string{"live_ops"}, nil)
	body := fmt.Sprintf(
		`{"base_release_id":%d,"versions":[{"namespace":%q,"version":%d}],"packs":[],"min_client_version":"1.4.0","message":"race"}`,
		original, name, version)

	const writers = 2
	statuses := make([]int, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
				"http://"+h.public+"/api/admin/config/channels/dev/releases", strings.NewReader(body))
			if err != nil {
				statuses[i] = -1
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := h.client.Do(req)
			if err != nil {
				statuses[i] = -1
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			statuses[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	var created, stale int
	for _, s := range statuses {
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			stale++
		default:
			t.Errorf("concurrent publish status = %d, want 201 or 409", s)
		}
	}
	if created != 1 || stale != 1 {
		t.Errorf("outcomes = %d created / %d conflict, want 1 / 1", created, stale)
	}
}
