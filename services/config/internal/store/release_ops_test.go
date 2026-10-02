package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// publishOne creates a unique client namespace with one version and publishes it to ch
// on top of base. It returns the release and the namespace's name.
func publishOne(t *testing.T, db *store.DB, blobs *blob.Store, ch string, base int64, prefix, msg string) (store.Release, string) {
	t.Helper()

	ns, actor, version := publishFixture(t, db, blobs, prefix)
	rel, err := db.Publish(context.Background(), ch, store.PublishRequest{
		BaseReleaseID:    base,
		Versions:         []store.PublishVersion{{Namespace: ns, Version: version}},
		MinClientVersion: "1.0.0",
		Message:          msg,
	}, actor, blobs)
	if err != nil {
		t.Fatalf("Publish(%s): %v", ch, err)
	}
	return rel, ns
}

// manifestContent is the part of a manifest a promotion must copy exactly.
type manifestContent struct {
	Channel          string                     `json:"channel"`
	ReleaseID        int64                      `json:"release_id"`
	MinClientVersion string                     `json:"min_client_version"`
	Config           map[string]json.RawMessage `json:"config"`
	Packs            []json.RawMessage          `json:"packs"`
}

func readManifestContent(t *testing.T, db *store.DB, id int64) manifestContent {
	t.Helper()

	var raw []byte
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT manifest FROM release WHERE release_id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read manifest %d: %v", id, err)
	}
	var m manifestContent
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode manifest %d: %v", id, err)
	}
	return m
}

// TestListReleasesPagingHeadAndIsHead covers the history contract: newest first, the
// head marked and stated once, and a before cursor that pages strictly downward.
func TestListReleasesPagingHeadAndIsHead(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")
	t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	relA, _ := publishOne(t, db, blobs, "dev", original, "test.history_a", "A")
	relB, _ := publishOne(t, db, blobs, "dev", relA.ReleaseID, "test.history_b", "B")

	head, page, next, err := db.ListReleases(ctx, "dev", nil, 2)
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if head != relB.ReleaseID {
		t.Errorf("head_release_id = %d, want %d", head, relB.ReleaseID)
	}
	if len(page) != 2 {
		t.Fatalf("page size = %d, want 2", len(page))
	}
	if page[0].ReleaseID != relB.ReleaseID || page[1].ReleaseID != relA.ReleaseID {
		t.Errorf("page ids = %d,%d, want %d,%d", page[0].ReleaseID, page[1].ReleaseID, relB.ReleaseID, relA.ReleaseID)
	}
	if !page[0].IsHead || page[1].IsHead {
		t.Errorf("is_head = %v,%v, want true,false", page[0].IsHead, page[1].IsHead)
	}
	if page[0].Message != "B" || page[1].Message != "A" {
		t.Errorf("messages = %q,%q, want B,A", page[0].Message, page[1].Message)
	}

	// A full page means there may be more: the cursor is the last id returned, and the
	// next page is strictly below it.
	head2, older, next2, err := db.ListReleases(ctx, "dev", next, 1)
	if err != nil {
		t.Fatalf("ListReleases(page 2): %v", err)
	}
	if head2 != head {
		t.Errorf("second page head = %d, want %d", head2, head)
	}
	if len(older) != 1 || older[0].ReleaseID >= relA.ReleaseID {
		t.Fatalf("second page = %+v, want exactly one release below %d", older, relA.ReleaseID)
	}
	if older[0].IsHead {
		t.Error("an older release was marked as head")
	}
	if next2 != nil && *next2 != older[0].ReleaseID {
		t.Errorf("second page next_before = %v, want nil or %d", next2, older[0].ReleaseID)
	}
}

// TestRollbackMovesPointerAuditsAndNotifies is the rollback contract: the pointer moves
// to an earlier release of the same channel, no release row is written, one audit entry
// lands and Patch is notified.
func TestRollbackMovesPointerAuditsAndNotifies(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")
	t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	relA, _ := publishOne(t, db, blobs, "dev", original, "test.rollback_a", "A")
	relB, _ := publishOne(t, db, blobs, "dev", relA.ReleaseID, "test.rollback_b", "B")

	listener, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listener: %v", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, `LISTEN config_release`); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	before := countChannelReleases(t, db, "dev")
	actor := store.Entry{ActorID: "staff-rollback", ActorName: "Rollback Operator"}
	rel, err := db.Rollback(ctx, "dev", relA.ReleaseID, relB.ReleaseID, actor)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rel.ReleaseID != relA.ReleaseID || rel.Channel != "dev" {
		t.Errorf("rolled-back release = %+v, want release %d on dev", rel, relA.ReleaseID)
	}
	if rel.Message != "A" {
		t.Errorf("rolled-back message = %q, want A", rel.Message)
	}
	if got := channelHead(t, db, "dev"); got != relA.ReleaseID {
		t.Errorf("dev head = %d, want %d", got, relA.ReleaseID)
	}
	if got := countChannelReleases(t, db, "dev"); got != before {
		t.Errorf("release count = %d after rollback, want %d (no new row)", got, before)
	}

	var audits int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log
		  WHERE action = 'release.rollback' AND target = 'dev'
		    AND (details->>'from')::bigint = $1 AND (details->>'to')::bigint = $2`,
		relB.ReleaseID, relA.ReleaseID).Scan(&audits); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("%d release.rollback audit rows, want 1", audits)
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

	// A release belonging to another channel is a 404, not a rollback.
	lockChannel(t, db, "staging")
	stagingOriginal := channelHead(t, db, "staging")
	_, err = db.Rollback(ctx, "dev", stagingOriginal, relA.ReleaseID, actor)
	var notIn *store.ReleaseNotInChannelError
	if !errors.As(err, &notIn) {
		t.Fatalf("rollback to a staging release = %v, want ReleaseNotInChannelError", err)
	}
	if notIn.ReleaseID != stagingOriginal || notIn.Channel != "dev" {
		t.Errorf("not-in-channel = %+v, want release %d on dev", notIn, stagingOriginal)
	}
	if got := channelHead(t, db, "dev"); got != relA.ReleaseID {
		t.Errorf("head moved to %d after a rejected rollback, want %d", got, relA.ReleaseID)
	}

	// Rolling back to the head is a no-op conflict.
	if _, err := db.Rollback(ctx, "dev", relA.ReleaseID, relA.ReleaseID, actor); !errors.Is(err, store.ErrNoReleaseChanges) {
		t.Errorf("rollback to head = %v, want ErrNoReleaseChanges", err)
	}

	// A base that is not the head is stale.
	if _, err := db.Rollback(ctx, "dev", relA.ReleaseID, original, actor); !errors.Is(err, store.ErrStaleRelease) {
		t.Errorf("stale rollback = %v, want ErrStaleRelease", err)
	}
	if got := channelHead(t, db, "dev"); got != relA.ReleaseID {
		t.Errorf("head = %d after stale rollback, want %d", got, relA.ReleaseID)
	}
}

// TestPromoteCopiesContentAndNotifies is the promotion contract: a new release on the
// upper channel with the lower channel's content, a fresh id, the exact Publish hash
// rule, and a notification; promoting the same content again is a no-op conflict.
func TestPromoteCopiesContentAndNotifies(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	lockChannel(t, db, "staging")
	devOriginal := channelHead(t, db, "dev")
	stagingOriginal := channelHead(t, db, "staging")
	t.Cleanup(func() {
		restoreChannelHead(t, db, "dev", devOriginal)
		restoreChannelHead(t, db, "staging", stagingOriginal)
	})

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	devRelease, ns := publishOne(t, db, blobs, "dev", devOriginal, "test.promote", "dev content")

	listener, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listener: %v", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, `LISTEN config_release`); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	before := countChannelReleases(t, db, "staging")
	actor := store.Entry{ActorID: "staff-promote", ActorName: "Promote Operator"}
	rel, err := db.Promote(ctx, "staging", "dev", stagingOriginal, "", actor)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if rel.Channel != "staging" {
		t.Errorf("promoted channel = %q, want staging", rel.Channel)
	}
	if rel.ReleaseID <= devRelease.ReleaseID {
		t.Errorf("promoted release_id = %d, want a fresh id above %d", rel.ReleaseID, devRelease.ReleaseID)
	}
	if rel.Message != "promote dev release "+strconv.FormatInt(devRelease.ReleaseID, 10) {
		t.Errorf("default message = %q", rel.Message)
	}
	if got := channelHead(t, db, "staging"); got != rel.ReleaseID {
		t.Errorf("staging head = %d, want %d", got, rel.ReleaseID)
	}
	if got := countChannelReleases(t, db, "staging"); got != before+1 {
		t.Errorf("staging release count = %d, want %d", got, before+1)
	}

	// The stored manifest must obey the same canonical-hash rule Publish uses.
	var (
		storedText string
		storedSHA  string
	)
	if err := db.Pool.QueryRow(ctx,
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

	promoted := readManifestContent(t, db, rel.ReleaseID)
	source := readManifestContent(t, db, devRelease.ReleaseID)
	if promoted.Channel != "staging" {
		t.Errorf("manifest channel = %q, want staging", promoted.Channel)
	}
	if promoted.ReleaseID != rel.ReleaseID {
		t.Errorf("manifest release_id = %d, want %d", promoted.ReleaseID, rel.ReleaseID)
	}
	if promoted.MinClientVersion != source.MinClientVersion {
		t.Errorf("manifest min_client_version = %q, want %q", promoted.MinClientVersion, source.MinClientVersion)
	}
	if !reflect.DeepEqual(promoted.Config, source.Config) {
		t.Errorf("manifest config = %v, want %v", promoted.Config, source.Config)
	}
	if !reflect.DeepEqual(promoted.Packs, source.Packs) {
		t.Errorf("manifest packs = %v, want %v", promoted.Packs, source.Packs)
	}
	if _, ok := promoted.Config[ns]; !ok {
		t.Errorf("manifest config has no entry for the promoted namespace %s", ns)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	notification, err := listener.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if notification.Payload != "staging" {
		t.Errorf("notification payload = %q, want staging", notification.Payload)
	}

	var audits int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log
		  WHERE action = 'release.promote' AND target = 'staging'
		    AND details->>'from_channel' = 'dev'
		    AND (details->>'source_release_id')::bigint = $1
		    AND (details->>'release_id')::bigint = $2`,
		devRelease.ReleaseID, rel.ReleaseID).Scan(&audits); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("%d release.promote audit rows, want 1", audits)
	}

	// Promoting the same content onto its new head is a no-op conflict.
	if _, err := db.Promote(ctx, "staging", "dev", rel.ReleaseID, "", actor); !errors.Is(err, store.ErrNoReleaseChanges) {
		t.Errorf("re-promote = %v, want ErrNoReleaseChanges", err)
	}

	// A stale base is refused and writes nothing.
	beforeStale := countChannelReleases(t, db, "staging")
	if _, err := db.Promote(ctx, "staging", "dev", stagingOriginal, "", actor); !errors.Is(err, store.ErrStaleRelease) {
		t.Errorf("stale promote = %v, want ErrStaleRelease", err)
	}
	if got := countChannelReleases(t, db, "staging"); got != beforeStale {
		t.Errorf("staging release count = %d after stale promote, want %d", got, beforeStale)
	}
}

// insertLegacyRelease writes a release row the way migration 00002 did: the server
// manifest columns are left NULL, which is what a release predating server manifests
// looks like.
func insertLegacyRelease(t *testing.T, db *store.DB, ch, minVersion, message string) int64 {
	t.Helper()

	ctx := context.Background()
	var id int64
	if err := db.Pool.QueryRow(ctx,
		`SELECT nextval(pg_get_serial_sequence('release','release_id'))`).Scan(&id); err != nil {
		t.Fatalf("next release id: %v", err)
	}
	body := fmt.Sprintf(
		`{"channel":%q,"config":{},"format":1,"min_client_version":%q,"packs":[],"release_id":%d}`,
		ch, minVersion, id)
	canonical, err := schema.Canonical([]byte(body))
	if err != nil {
		t.Fatalf("Canonical(legacy): %v", err)
	}
	sum := sha256.Sum256(canonical)
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO release (release_id, channel, manifest, manifest_sha256,
		                      min_client_version, message, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, 'test-legacy')`,
		id, ch, canonical, hex.EncodeToString(sum[:]), minVersion, message); err != nil {
		t.Fatalf("insert legacy release: %v", err)
	}
	return id
}

// TestRollbackCarriesServerManifest proves the Release Rollback returns includes the
// target row's server manifest, and that rolling back to a legacy row (NULL server
// columns) comes back with no server manifest rather than an error.
func TestRollbackCarriesServerManifest(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	original := channelHead(t, db, "dev")
	t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	clientNS, actor, clientVersion := publishFixture(t, db, blobs, "test.rollback_server_client")
	serverNS, serverVersion := serverPublishFixture(t, db, blobs, "test.rollback_server_srv", actor)
	relA, err := db.Publish(ctx, "dev", store.PublishRequest{
		BaseReleaseID: original,
		Versions: []store.PublishVersion{
			{Namespace: clientNS, Version: clientVersion},
			{Namespace: serverNS, Version: serverVersion},
		},
		MinClientVersion: "1.0.0",
		Message:          "A",
	}, actor, blobs)
	if err != nil {
		t.Fatalf("Publish A: %v", err)
	}
	relB, _ := publishOne(t, db, blobs, "dev", relA.ReleaseID, "test.rollback_server_b", "B")

	rolled, err := db.Rollback(ctx, "dev", relA.ReleaseID, relB.ReleaseID, actor)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolled.ServerManifest == nil || rolled.ServerManifestSHA256 == "" {
		t.Fatalf("rolled-back server manifest = %v / %q, want the target's",
			rolled.ServerManifest, rolled.ServerManifestSHA256)
	}
	got := sha256.Sum256(rolled.ServerManifest)
	if hex.EncodeToString(got[:]) != rolled.ServerManifestSHA256 {
		t.Errorf("rolled-back server manifest hash = %s, want the hash of its bytes",
			rolled.ServerManifestSHA256)
	}

	// A legacy target has nil bytes and an empty hash.
	legacyID := insertLegacyRelease(t, db, "dev", "0.0.0", "legacy")
	legacy, err := db.Rollback(ctx, "dev", legacyID, relA.ReleaseID, actor)
	if err != nil {
		t.Fatalf("Rollback to legacy: %v", err)
	}
	if legacy.ServerManifest != nil || legacy.ServerManifestSHA256 != "" {
		t.Errorf("legacy rollback server manifest = %v / %q, want nil / empty",
			legacy.ServerManifest, legacy.ServerManifestSHA256)
	}
}

// TestPromoteCopiesServerConfig proves promotion carries the source's server config, and
// that a change only in the server config is a real change rather than
// ErrNoReleaseChanges.
func TestPromoteCopiesServerConfig(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	lockChannel(t, db, "staging")
	devOriginal := channelHead(t, db, "dev")
	stagingOriginal := channelHead(t, db, "staging")
	t.Cleanup(func() {
		restoreChannelHead(t, db, "dev", devOriginal)
		restoreChannelHead(t, db, "staging", stagingOriginal)
	})

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// One client version and two server versions, so the two dev releases can share
	// client content and differ only in server content.
	clientNS, actor, clientVersion := publishFixture(t, db, blobs, "test.promote_server_client")
	serverNS := uniqueName("test.promote_server_srv")
	if _, err := db.CreateNamespace(ctx, serverNS, "server", "", actor); err != nil {
		t.Fatalf("CreateNamespace(server): %v", err)
	}
	if _, err := db.ReplaceSchema(ctx, serverNS, []byte(`{"type":"object"}`), nil, actor); err != nil {
		t.Fatalf("ReplaceSchema(server): %v", err)
	}
	if _, err := db.SaveDraft(ctx, serverNS, []byte(`{"level":1}`), 1, actor); err != nil {
		t.Fatalf("SaveDraft(server v1): %v", err)
	}
	v1, err := db.CreateVersion(ctx, serverNS, 2, "v1", actor, variantPrepare(blobs))
	if err != nil {
		t.Fatalf("CreateVersion(server v1): %v", err)
	}
	if _, err := db.SaveDraft(ctx, serverNS, []byte(`{"level":2}`), 2, actor); err != nil {
		t.Fatalf("SaveDraft(server v2): %v", err)
	}
	v2, err := db.CreateVersion(ctx, serverNS, 3, "v2", actor, variantPrepare(blobs))
	if err != nil {
		t.Fatalf("CreateVersion(server v2): %v", err)
	}

	dev1, err := db.Publish(ctx, "dev", store.PublishRequest{
		BaseReleaseID: devOriginal,
		Versions: []store.PublishVersion{
			{Namespace: clientNS, Version: clientVersion},
			{Namespace: serverNS, Version: v1.Version},
		},
		MinClientVersion: "1.0.0",
		Message:          "one",
	}, actor, blobs)
	if err != nil {
		t.Fatalf("Publish one: %v", err)
	}
	promoted1, err := db.Promote(ctx, "staging", "dev", stagingOriginal, "", actor)
	if err != nil {
		t.Fatalf("Promote one: %v", err)
	}
	server1 := decodeManifest(t, promoted1.ServerManifest)
	if got, ok := server1.Config[serverNS]; !ok || got.Version != v1.Version {
		t.Errorf("promoted server config[%s] = %+v, want version %d", serverNS, got, v1.Version)
	}
	if _, leaked := server1.Config[clientNS]; leaked {
		t.Errorf("client namespace %s leaked into the promoted server manifest", clientNS)
	}

	// The second dev release keeps the same client content and changes only the server
	// config: promoting it must be a real change.
	if _, err := db.Publish(ctx, "dev", store.PublishRequest{
		BaseReleaseID: dev1.ReleaseID,
		Versions: []store.PublishVersion{
			{Namespace: clientNS, Version: clientVersion},
			{Namespace: serverNS, Version: v2.Version},
		},
		MinClientVersion: "1.0.0",
		Message:          "two",
	}, actor, blobs); err != nil {
		t.Fatalf("Publish two: %v", err)
	}
	promoted2, err := db.Promote(ctx, "staging", "dev", promoted1.ReleaseID, "", actor)
	if err != nil {
		t.Fatalf("Promote two = %v, want success when only the server config differs", err)
	}
	server2 := decodeManifest(t, promoted2.ServerManifest)
	if got, ok := server2.Config[serverNS]; !ok || got.Version != v2.Version {
		t.Errorf("second promoted server config[%s] = %+v, want version %d", serverNS, got, v2.Version)
	}

	// Now the client and server content both match the head: a no-op.
	if _, err := db.Promote(ctx, "staging", "dev", promoted2.ReleaseID, "", actor); !errors.Is(err, store.ErrNoReleaseChanges) {
		t.Errorf("re-promote = %v, want ErrNoReleaseChanges", err)
	}
}

// TestPromoteFromLegacyReleaseBuildsEmptyServerConfig proves a source row with NULL
// server columns promotes as an empty server config rather than failing.
func TestPromoteFromLegacyReleaseBuildsEmptyServerConfig(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	lockChannel(t, db, "dev")
	lockChannel(t, db, "staging")
	devOriginal := channelHead(t, db, "dev")
	stagingOriginal := channelHead(t, db, "staging")
	t.Cleanup(func() {
		restoreChannelHead(t, db, "dev", devOriginal)
		restoreChannelHead(t, db, "staging", stagingOriginal)
	})

	// A distinct min_client_version keeps this a real change against whatever content
	// the staging head already carries.
	legacyID := insertLegacyRelease(t, db, "dev", "9.9.9", "legacy source")
	if _, err := db.Pool.Exec(ctx,
		`UPDATE channel_head SET release_id = $1, updated_by = 'test-legacy', updated_at = now()
		  WHERE channel = 'dev'`, legacyID); err != nil {
		t.Fatalf("move dev head to legacy: %v", err)
	}

	actor := store.Entry{ActorID: "staff-promote-legacy", ActorName: "Promote Operator"}
	promoted, err := db.Promote(ctx, "staging", "dev", stagingOriginal, "", actor)
	if err != nil {
		t.Fatalf("Promote from legacy: %v", err)
	}
	if promoted.ServerManifest == nil || promoted.ServerManifestSHA256 == "" {
		t.Fatalf("promoted server manifest = %v / %q, want a built empty manifest",
			promoted.ServerManifest, promoted.ServerManifestSHA256)
	}
	server := decodeManifest(t, promoted.ServerManifest)
	if len(server.Config) != 0 {
		t.Errorf("promoted server config = %v, want empty", server.Config)
	}
	if server.Packs == nil || len(server.Packs) != 0 {
		t.Errorf("promoted server packs = %v, want []", server.Packs)
	}
}
