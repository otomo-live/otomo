package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// lockChannel serialises the tests that publish to a global channel. Go runs the test
// binaries of different packages concurrently, and channel_head is shared state; a
// session advisory lock keeps one package's publish from moving the head out from under
// another's preview.
func lockChannel(t *testing.T, db *store.DB, ch string) {
	t.Helper()

	ctx := context.Background()
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a lock connection: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1)::bigint)`, "config_test:"+ch); err != nil {
		conn.Release()
		t.Fatalf("advisory lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1)::bigint)`, "config_test:"+ch)
		conn.Release()
	})
}

func channelHead(t *testing.T, db *store.DB, ch string) int64 {
	t.Helper()
	var id int64
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT release_id FROM channel_head WHERE channel = $1`, ch).Scan(&id); err != nil {
		t.Fatalf("read channel head %s: %v", ch, err)
	}
	return id
}

func restoreChannelHead(t *testing.T, db *store.DB, ch string, id int64) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(),
		`UPDATE channel_head SET release_id = $2, updated_by = 'test-cleanup', updated_at = now()
		  WHERE channel = $1`, ch, id); err != nil {
		t.Fatalf("restore channel head %s: %v", ch, err)
	}
}

func countChannelReleases(t *testing.T, db *store.DB, ch string) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM release WHERE channel = $1`, ch).Scan(&n); err != nil {
		t.Fatalf("count releases: %v", err)
	}
	return n
}

// publishFixture creates a client namespace with one immutable version, and returns the
// namespace, its actor and the version number.
func publishFixture(t *testing.T, db *store.DB, blobs *blob.Store, prefix string) (string, store.Entry, int) {
	t.Helper()
	name, actor := createTestNamespace(t, db, prefix)
	if _, err := db.ReplaceSchema(context.Background(), name, []byte(`{"type":"object"}`), nil, actor); err != nil {
		t.Fatalf("ReplaceSchema: %v", err)
	}
	if _, err := db.SaveDraft(context.Background(), name, []byte(`{"level":1}`), 1, actor); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	v, err := db.CreateVersion(context.Background(), name, 2, "v1", actor, variantPrepare(blobs))
	if err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	return name, actor, v.Version
}

// addPack uploads a pack's bytes to the blob store and records the content_pack row.
func addPack(t *testing.T, db *store.DB, blobs *blob.Store, actor store.Entry, body []byte) blob.Ref {
	t.Helper()
	ref, err := blobs.PutBytes(context.Background(), body)
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if _, _, err := db.CreatePack(context.Background(), uniqueName("test_pack"), ref, actor); err != nil {
		t.Fatalf("CreatePack: %v", err)
	}
	return ref
}

// assertStoredManifestCanonical is the exact check Patch performs: re-canonicalise the
// stored manifest text and compare its hash to the column.
func assertStoredManifestCanonical(t *testing.T, db *store.DB, releaseID int64) []byte {
	t.Helper()

	var (
		text string
		sum  string
	)
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT manifest::text, manifest_sha256 FROM release WHERE release_id = $1`, releaseID).Scan(&text, &sum); err != nil {
		t.Fatalf("read stored manifest: %v", err)
	}
	canonical, err := schema.Canonical([]byte(text))
	if err != nil {
		t.Fatalf("Canonical(stored): %v", err)
	}
	got := sha256.Sum256(canonical)
	if hex.EncodeToString(got[:]) != sum {
		t.Errorf("manifest_sha256 = %s, canonical stored manifest hashes to %s",
			sum, hex.EncodeToString(got[:]))
	}
	return canonical
}

// assertStoredServerManifestCanonical is the server-manifest twin of the check above.
func assertStoredServerManifestCanonical(t *testing.T, db *store.DB, releaseID int64) []byte {
	t.Helper()

	var (
		text string
		sum  string
	)
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT server_manifest::text, server_manifest_sha256 FROM release WHERE release_id = $1`, releaseID).Scan(&text, &sum); err != nil {
		t.Fatalf("read stored server manifest: %v", err)
	}
	canonical, err := schema.Canonical([]byte(text))
	if err != nil {
		t.Fatalf("Canonical(stored server): %v", err)
	}
	got := sha256.Sum256(canonical)
	if hex.EncodeToString(got[:]) != sum {
		t.Errorf("server_manifest_sha256 = %s, canonical stored server manifest hashes to %s",
			sum, hex.EncodeToString(got[:]))
	}
	return canonical
}

// manifestView is the decoded manifest shape the tests assert on.
type manifestView struct {
	Format           int                           `json:"format"`
	Channel          string                        `json:"channel"`
	ReleaseID        int64                         `json:"release_id"`
	CreatedAt        string                        `json:"created_at"`
	MinClientVersion string                        `json:"min_client_version"`
	Config           map[string]manifestConfigView `json:"config"`
	Packs            []manifestPackView            `json:"packs"`
}

type manifestConfigView struct {
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

type manifestPackView struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func decodeManifest(t *testing.T, raw []byte) manifestView {
	t.Helper()

	var m manifestView
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	return m
}

// serverPublishFixture creates a server-audience namespace with one immutable version
// and returns the namespace and its version number.
func serverPublishFixture(t *testing.T, db *store.DB, blobs *blob.Store, prefix string, actor store.Entry) (string, int) {
	t.Helper()

	name := uniqueName(prefix)
	if _, err := db.CreateNamespace(context.Background(), name, "server", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if _, err := db.ReplaceSchema(context.Background(), name, []byte(`{"type":"object"}`), nil, actor); err != nil {
		t.Fatalf("ReplaceSchema: %v", err)
	}
	if _, err := db.SaveDraft(context.Background(), name, []byte(`{"level":1}`), 1, actor); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	v, err := db.CreateVersion(context.Background(), name, 2, "v1", actor, variantPrepare(blobs))
	if err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	return name, v.Version
}

// versionEntryExpectation reads the sha and on-disk size a manifest entry for ns at
// version must carry.
func versionEntryExpectation(t *testing.T, db *store.DB, blobs *blob.Store, ns string, version int) (string, int64) {
	t.Helper()

	var sha string
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT sha256 FROM config_version WHERE namespace = $1 AND version = $2`, ns, version).Scan(&sha); err != nil {
		t.Fatalf("read version sha: %v", err)
	}
	rc, size, err := blobs.Open(sha)
	if err != nil {
		t.Fatalf("open blob %s: %v", sha, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close blob %s: %v", sha, err)
	}
	return sha, size
}

// TestPublishHappyPath is the store-level contract: the manifest carries each item's
// on-disk size and canonical hash, the head moves, and exactly one audit row lands.
func TestPublishHappyPath(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")
	t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ns, actor, version := publishFixture(t, db, blobs, "test.publish")
	pack := addPack(t, db, blobs, actor, []byte("GDPC-payload"))

	before := countChannelReleases(t, db, "dev")
	rel, err := db.Publish(ctx, "dev", store.PublishRequest{
		BaseReleaseID:    original,
		Versions:         []store.PublishVersion{{Namespace: ns, Version: version}},
		PackSHA256:       []string{pack.SHA256},
		MinClientVersion: "1.4.0",
		Message:          "nerf the sword",
	}, actor, blobs)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if rel.ReleaseID <= original {
		t.Errorf("release_id = %d, want greater than the old head %d", rel.ReleaseID, original)
	}
	if rel.Channel != "dev" || rel.MinClientVersion != "1.4.0" || rel.Message != "nerf the sword" {
		t.Errorf("release = %+v", rel)
	}
	if rel.CreatedBy != actor.ActorID || rel.CreatedAt.IsZero() {
		t.Errorf("release attribution = %q/%v, want %q/non-zero", rel.CreatedBy, rel.CreatedAt, actor.ActorID)
	}
	if got := channelHead(t, db, "dev"); got != rel.ReleaseID {
		t.Errorf("channel head = %d, want %d", got, rel.ReleaseID)
	}
	if after := countChannelReleases(t, db, "dev"); after != before+1 {
		t.Errorf("release count = %d, want %d", after, before+1)
	}

	b := assertStoredManifestCanonical(t, db, rel.ReleaseID)
	var manifest struct {
		Format           int    `json:"format"`
		Channel          string `json:"channel"`
		ReleaseID        int64  `json:"release_id"`
		CreatedAt        string `json:"created_at"`
		MinClientVersion string `json:"min_client_version"`
		Config           map[string]struct {
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
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	if manifest.Format != 1 || manifest.Channel != "dev" || manifest.ReleaseID != rel.ReleaseID {
		t.Errorf("manifest header = %+v", manifest)
	}
	if _, err := time.Parse(time.RFC3339, manifest.CreatedAt); err != nil {
		t.Errorf("manifest created_at %q is not RFC3339: %v", manifest.CreatedAt, err)
	}
	if manifest.MinClientVersion != "1.4.0" {
		t.Errorf("manifest min_client_version = %q", manifest.MinClientVersion)
	}
	gotCfg, ok := manifest.Config[ns]
	if !ok {
		t.Fatalf("manifest config has no entry for %s", ns)
	}
	if gotCfg.Version != version || gotCfg.SHA256 == "" || gotCfg.Size <= 0 {
		t.Errorf("manifest config[%s] = %+v", ns, gotCfg)
	}
	if len(manifest.Packs) != 1 || manifest.Packs[0].SHA256 != pack.SHA256 || manifest.Packs[0].Size != pack.Size {
		t.Errorf("manifest packs = %+v, want %s size %d", manifest.Packs, pack.SHA256, pack.Size)
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
}

// TestPublishStaleReleaseWritesNothing proves the row lock's check: a base the channel
// has moved past is a 409, and neither the head nor the release table changes.
func TestPublishStaleReleaseWritesNothing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")
	t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ns, actor, version := publishFixture(t, db, blobs, "test.publish_stale")
	req := store.PublishRequest{
		BaseReleaseID:    original,
		Versions:         []store.PublishVersion{{Namespace: ns, Version: version}},
		MinClientVersion: "1.0.0",
		Message:          "first",
	}
	if _, err := db.Publish(ctx, "dev", req, actor, blobs); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	headAfterFirst := channelHead(t, db, "dev")
	releasesAfterFirst := countChannelReleases(t, db, "dev")

	req.Message = "second"
	_, err = db.Publish(ctx, "dev", req, actor, blobs)
	var stale *store.StaleReleaseError
	if !errors.As(err, &stale) {
		t.Fatalf("stale Publish = %v, want StaleReleaseError", err)
	}
	if stale.Base != original || stale.Current != headAfterFirst {
		t.Errorf("stale = %+v, want base %d current %d", stale, original, headAfterFirst)
	}
	if got := channelHead(t, db, "dev"); got != headAfterFirst {
		t.Errorf("head = %d after a stale publish, want %d", got, headAfterFirst)
	}
	if got := countChannelReleases(t, db, "dev"); got != releasesAfterFirst {
		t.Errorf("release count = %d after a stale publish, want %d", got, releasesAfterFirst)
	}
}

// TestPublishIsSerialised is the concurrency contract: two publishes with the same base
// produce exactly one release; the loser sees the winner's head and gets a stale error.
func TestPublishIsSerialised(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")
	t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ns, actor, version := publishFixture(t, db, blobs, "test.publish_race")
	req := store.PublishRequest{
		BaseReleaseID:    original,
		Versions:         []store.PublishVersion{{Namespace: ns, Version: version}},
		MinClientVersion: "1.0.0",
		Message:          "race",
	}
	before := countChannelReleases(t, db, "dev")

	const writers = 2
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = db.Publish(ctx, "dev", req, actor, blobs)
		}(i)
	}
	wg.Wait()

	var wins, stales int
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrStaleRelease):
			stales++
		default:
			t.Errorf("concurrent Publish %d: %v", i, err)
		}
	}
	if wins != 1 || stales != writers-1 {
		t.Errorf("outcomes = %d wins / %d stale, want 1 / %d", wins, stales, writers-1)
	}
	if got := countChannelReleases(t, db, "dev"); got != before+1 {
		t.Errorf("release count = %d, want %d", got, before+1)
	}
}

// TestPublishSplitsServerNamespaces proves a publish may carry server-audience
// namespaces: they land in the server manifest only, the client manifest keeps just the
// client namespaces, and both documents share one release id, channel and timestamp.
func TestPublishSplitsServerNamespaces(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")
	t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	clientNS, actor, clientVersion := publishFixture(t, db, blobs, "test.publish_split_client")
	serverNS, serverVersion := serverPublishFixture(t, db, blobs, "test.publish_split_server", actor)
	clientSHA, clientSize := versionEntryExpectation(t, db, blobs, clientNS, clientVersion)
	serverSHA, serverSize := versionEntryExpectation(t, db, blobs, serverNS, serverVersion)

	rel, err := db.Publish(ctx, "dev", store.PublishRequest{
		BaseReleaseID: original,
		Versions: []store.PublishVersion{
			{Namespace: clientNS, Version: clientVersion},
			{Namespace: serverNS, Version: serverVersion},
		},
		MinClientVersion: "1.2.0",
		Message:          "split the audiences",
	}, actor, blobs)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	client := decodeManifest(t, rel.Manifest)
	server := decodeManifest(t, rel.ServerManifest)

	if len(client.Config) != 1 {
		t.Errorf("client manifest config = %v, want only %s", client.Config, clientNS)
	}
	if _, leaked := client.Config[serverNS]; leaked {
		t.Errorf("server namespace %s leaked into the client manifest", serverNS)
	}
	gotClient, ok := client.Config[clientNS]
	if !ok || gotClient.Version != clientVersion || gotClient.SHA256 != clientSHA || gotClient.Size != clientSize {
		t.Errorf("client config[%s] = %+v, want version %d sha %s size %d",
			clientNS, gotClient, clientVersion, clientSHA, clientSize)
	}

	if len(server.Config) != 1 {
		t.Errorf("server manifest config = %v, want only %s", server.Config, serverNS)
	}
	if _, leaked := server.Config[clientNS]; leaked {
		t.Errorf("client namespace %s leaked into the server manifest", clientNS)
	}
	gotServer, ok := server.Config[serverNS]
	if !ok || gotServer.Version != serverVersion || gotServer.SHA256 != serverSHA || gotServer.Size != serverSize {
		t.Errorf("server config[%s] = %+v, want version %d sha %s size %d",
			serverNS, gotServer, serverVersion, serverSHA, serverSize)
	}
	if server.Packs == nil || len(server.Packs) != 0 {
		t.Errorf("server manifest packs = %v, want []", server.Packs)
	}

	// The two documents describe the same release moment.
	if client.ReleaseID != rel.ReleaseID || server.ReleaseID != rel.ReleaseID {
		t.Errorf("manifest release_ids = %d/%d, want %d", client.ReleaseID, server.ReleaseID, rel.ReleaseID)
	}
	if client.Channel != "dev" || server.Channel != "dev" {
		t.Errorf("manifest channels = %q/%q, want dev", client.Channel, server.Channel)
	}
	if client.CreatedAt != server.CreatedAt {
		t.Errorf("created_at = %q/%q, want the two manifests to agree", client.CreatedAt, server.CreatedAt)
	}

	// Each returned hash is the SHA-256 of that manifest's returned canonical bytes,
	// and of the bytes stored in its column.
	if rel.ServerManifestSHA256 == "" {
		t.Fatal("ServerManifestSHA256 is empty")
	}
	if got := sha256.Sum256(rel.ServerManifest); hex.EncodeToString(got[:]) != rel.ServerManifestSHA256 {
		t.Errorf("ServerManifestSHA256 = %s, want the hash of ServerManifest", rel.ServerManifestSHA256)
	}
	if got := sha256.Sum256(rel.Manifest); hex.EncodeToString(got[:]) != rel.ManifestSHA256 {
		t.Errorf("ManifestSHA256 = %s, want the hash of Manifest", rel.ManifestSHA256)
	}
	assertStoredManifestCanonical(t, db, rel.ReleaseID)
	assertStoredServerManifestCanonical(t, db, rel.ReleaseID)

	// The audit keeps every selection under "versions" and repeats the server ones
	// under "server_versions".
	var details []byte
	if err := db.Pool.QueryRow(ctx,
		`SELECT details FROM audit_log
		  WHERE action = 'release.publish' AND target = 'dev'
		    AND (details->>'release_id')::bigint = $1`, rel.ReleaseID).Scan(&details); err != nil {
		t.Fatalf("read publish audit details: %v", err)
	}
	var audit struct {
		Versions       []map[string]any `json:"versions"`
		ServerVersions []map[string]any `json:"server_versions"`
	}
	if err := json.Unmarshal(details, &audit); err != nil {
		t.Fatalf("audit details are not JSON: %v", err)
	}
	if len(audit.Versions) != 2 {
		t.Errorf("audit versions = %v, want both selections", audit.Versions)
	}
	if len(audit.ServerVersions) != 1 || audit.ServerVersions[0]["namespace"] != serverNS {
		t.Errorf("audit server_versions = %v, want only %s", audit.ServerVersions, serverNS)
	}
}

// TestPublishWithoutServerNamespacesKeepsClientManifest pins the client document's exact
// field set for a release with no server namespaces, and proves a server manifest is
// still built with an empty config and an empty packs array.
func TestPublishWithoutServerNamespacesKeepsClientManifest(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")
	t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ns, actor, version := publishFixture(t, db, blobs, "test.publish_noserver")

	rel, err := db.Publish(ctx, "dev", store.PublishRequest{
		BaseReleaseID:    original,
		Versions:         []store.PublishVersion{{Namespace: ns, Version: version}},
		MinClientVersion: "1.0.0",
		Message:          "client only",
	}, actor, blobs)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(rel.Manifest, &top); err != nil {
		t.Fatalf("client manifest is not JSON: %v", err)
	}
	want := map[string]bool{
		"format": true, "channel": true, "release_id": true, "created_at": true,
		"min_client_version": true, "config": true, "packs": true,
	}
	for k := range top {
		if !want[k] {
			t.Errorf("unexpected client manifest key %q", k)
		}
	}
	for k := range want {
		if _, ok := top[k]; !ok {
			t.Errorf("client manifest is missing key %q", k)
		}
	}
	if len(top) != len(want) {
		t.Errorf("client manifest keys = %v, want exactly %v", top, want)
	}

	if rel.ServerManifest == nil {
		t.Fatal("ServerManifest is nil, want an empty server manifest")
	}
	if rel.ServerManifestSHA256 == "" {
		t.Error("ServerManifestSHA256 is empty")
	}
	server := decodeManifest(t, rel.ServerManifest)
	if len(server.Config) != 0 {
		t.Errorf("server manifest config = %v, want empty", server.Config)
	}
	if server.Packs == nil || len(server.Packs) != 0 {
		t.Errorf("server manifest packs = %v, want []", server.Packs)
	}
	assertStoredServerManifestCanonical(t, db, rel.ReleaseID)
}

// TestPublishUnknownTargets covers the three unresolved-selection cases.
func TestPublishUnknownTargets(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ns, actor, _ := publishFixture(t, db, blobs, "test.publish_unknown")
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")

	unknownSHA := ""
	for i := 0; i < 64; i++ {
		unknownSHA += "a"
	}

	tests := []struct {
		name string
		req  store.PublishRequest
		kind string
	}{
		{
			name: "namespace",
			req: store.PublishRequest{
				BaseReleaseID: original,
				Versions:      []store.PublishVersion{{Namespace: uniqueName("test.missing_ns"), Version: 1}},
			},
			kind: "namespace",
		},
		{
			name: "version",
			req: store.PublishRequest{
				BaseReleaseID: original,
				Versions:      []store.PublishVersion{{Namespace: ns, Version: 999}},
			},
			kind: "version",
		},
		{
			name: "pack",
			req: store.PublishRequest{
				BaseReleaseID: original,
				PackSHA256:    []string{unknownSHA},
			},
			kind: "pack",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.MinClientVersion = "1.0.0"
			tt.req.Message = "x"
			_, err := db.Publish(ctx, "dev", tt.req, actor, blobs)
			var unknown *store.UnknownTargetError
			if !errors.As(err, &unknown) {
				t.Fatalf("Publish = %v, want UnknownTargetError", err)
			}
			if unknown.Kind != tt.kind {
				t.Errorf("unknown kind = %q, want %q", unknown.Kind, tt.kind)
			}
		})
	}
}

// TestPublishMissingBlobIsAServerFault proves the release row is not written when the
// bytes it names are not on disk.
func TestPublishMissingBlobIsAServerFault(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")
	t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ns, actor, version := publishFixture(t, db, blobs, "test.publish_noblob")
	var sha string
	if err := db.Pool.QueryRow(ctx,
		`SELECT sha256 FROM config_version WHERE namespace = $1 AND version = $2`, ns, version).Scan(&sha); err != nil {
		t.Fatalf("read version sha: %v", err)
	}
	if err := os.Remove(blobPath(blobs.Root(), sha)); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	before := countChannelReleases(t, db, "dev")
	_, err = db.Publish(ctx, "dev", store.PublishRequest{
		BaseReleaseID:    original,
		Versions:         []store.PublishVersion{{Namespace: ns, Version: version}},
		MinClientVersion: "1.0.0",
		Message:          "x",
	}, actor, blobs)
	if err == nil {
		t.Fatal("Publish succeeded with a missing blob")
	}
	if errors.Is(err, store.ErrStaleRelease) || errors.Is(err, store.ErrReleaseTargetUnknown) {
		t.Errorf("missing blob surfaced as a client error: %v", err)
	}
	if got := countChannelReleases(t, db, "dev"); got != before {
		t.Errorf("release count = %d, want %d", got, before)
	}
}

// TestPublishNotifiesOnlyAfterCommit is CFG-B8's ordering: the LISTENer receives the
// channel after the publish commits, and nothing when the publish is stale.
func TestPublishNotifiesOnlyAfterCommit(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")
	t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ns, actor, version := publishFixture(t, db, blobs, "test.publish_notify")

	listener, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listener: %v", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, `LISTEN config_release`); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	req := store.PublishRequest{
		BaseReleaseID:    original,
		Versions:         []store.PublishVersion{{Namespace: ns, Version: version}},
		MinClientVersion: "1.0.0",
		Message:          "notify",
	}
	if _, err := db.Publish(ctx, "dev", req, actor, blobs); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	notification, err := listener.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if notification.Payload != "dev" {
		t.Errorf("notification payload = %q, want dev", notification.Payload)
	}
	secondCtx, secondCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer secondCancel()
	if _, err := listener.Conn().WaitForNotification(secondCtx); err == nil {
		t.Error("a second notification arrived after one publish")
	}

	// A stale publish must produce no notification at all.
	_, err = db.Publish(ctx, "dev", req, actor, blobs)
	if !errors.Is(err, store.ErrStaleRelease) {
		t.Fatalf("stale Publish = %v, want ErrStaleRelease", err)
	}
	staleCtx, staleCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer staleCancel()
	if _, err := listener.Conn().WaitForNotification(staleCtx); err == nil {
		t.Error("a stale publish produced a notification")
	}
}
