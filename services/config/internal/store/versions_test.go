package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// variantPrepare mirrors the API layer's prepare callback: validate the draft against
// the schema, canonicalise it, and write the canonical bytes to the blob store. It is
// what makes the store test exercise the same contract the handler does.
func variantPrepare(blobs *blob.Store) func([]byte, int, []byte) ([]byte, string, error) {
	return func(draftBody []byte, schemaVersion int, schemaBody []byte) ([]byte, string, error) {
		compiled, err := schema.Compile(schemaBody)
		if err != nil {
			return nil, "", err
		}
		issues, err := schema.Validate(compiled, draftBody)
		if err != nil {
			return nil, "", err
		}
		if len(issues) > 0 {
			return nil, "", fmt.Errorf("draft fails schema v%d: %v", schemaVersion, issues)
		}
		canonical, err := schema.Canonical(draftBody)
		if err != nil {
			return nil, "", err
		}
		sum := sha256.Sum256(canonical)
		sha := hex.EncodeToString(sum[:])
		ref, err := blobs.PutBytes(context.Background(), canonical)
		if err != nil {
			return nil, "", err
		}
		if ref.SHA256 != sha {
			return nil, "", fmt.Errorf("blob sha = %s, want %s", ref.SHA256, sha)
		}
		return canonical, sha, nil
	}
}

// blobPath is the fan-out path blob.Store uses: blobs/ab/cd/abcd….
func blobPath(root, sha string) string {
	return filepath.Join(root, "blobs", sha[0:2], sha[2:4], sha)
}

func countVersions(t *testing.T, db *store.DB, ns string) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM config_version WHERE namespace = $1`, ns).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}

func countVersionAudit(t *testing.T, db *store.DB, ns string) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'version.create' AND target = $1`, ns).Scan(&n); err != nil {
		t.Fatalf("count version.create audit rows: %v", err)
	}
	return n
}

// TestCreateVersion is the whole happy path plus its two conflict outcomes: a valid
// draft becomes v1 with the canonical bytes in the blob store, replaying it is
// ErrNoChanges, and a consumed revision is ErrStaleRevision.
func TestCreateVersion(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.version")

	schemaBody := []byte(`{"type":"object","properties":{"level":{"type":"integer"}},"required":["level"]}`)
	if _, err := db.ReplaceSchema(ctx, name, schemaBody, nil, actor); err != nil {
		t.Fatalf("ReplaceSchema: %v", err)
	}
	draft := []byte(`{"level":1}`)
	if _, err := db.SaveDraft(ctx, name, draft, 1, actor); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	v, err := db.CreateVersion(ctx, name, 2, "first version", actor, variantPrepare(blobs))
	if err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	if v.Namespace != name || v.Version != 1 || v.SchemaVersion != 2 {
		t.Errorf("version = %s v%d schema v%d, want %s v1 schema v2", v.Namespace, v.Version, v.SchemaVersion, name)
	}
	if v.Message != "first version" || v.CreatedBy != actor.ActorID {
		t.Errorf("version metadata = %q/%q, want first version/%q", v.Message, v.CreatedBy, actor.ActorID)
	}
	if v.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}

	canonical, err := schema.Canonical(draft)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	sum := sha256.Sum256(canonical)
	wantSHA := hex.EncodeToString(sum[:])
	if v.SHA256 != wantSHA {
		t.Errorf("sha256 = %s, want %s", v.SHA256, wantSHA)
	}
	if v.Size != len(canonical) {
		t.Errorf("size = %d, want %d", v.Size, len(canonical))
	}

	stored, err := os.ReadFile(blobPath(blobs.Root(), v.SHA256))
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(stored, canonical) {
		t.Errorf("blob bytes = %s, want %s", stored, canonical)
	}

	var base *int
	if err := db.Pool.QueryRow(ctx, `SELECT base_version FROM config_draft WHERE namespace = $1`, name).Scan(&base); err != nil {
		t.Fatalf("read base_version: %v", err)
	}
	if base == nil || *base != 1 {
		t.Errorf("base_version = %v, want 1", base)
	}
	if n := countVersionAudit(t, db, name); n != 1 {
		t.Errorf("%d version.create audit rows, want 1", n)
	}

	if _, err := db.CreateVersion(ctx, name, 2, "again", actor, variantPrepare(blobs)); !errors.Is(err, store.ErrNoChanges) {
		t.Errorf("replayed CreateVersion = %v, want ErrNoChanges", err)
	}
	if n := countVersions(t, db, name); n != 1 {
		t.Errorf("%d versions after a no-change replay, want 1", n)
	}

	if _, err := db.CreateVersion(ctx, name, 1, "stale", actor, variantPrepare(blobs)); !errors.Is(err, store.ErrStaleRevision) {
		t.Errorf("stale CreateVersion = %v, want ErrStaleRevision", err)
	}
}

// TestCreateVersionStaleRevisionWinsOverNoChanges pins the order of the two guards: a
// caller presenting an old revision gets stale_revision even when the body is
// unchanged, because staleness is what it can actually fix.
func TestCreateVersionStaleRevisionWinsOverNoChanges(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.version_stale")
	if _, err := db.SaveDraft(ctx, name, []byte(`{"level":2}`), 1, actor); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := db.CreateVersion(ctx, name, 2, "v1", actor, variantPrepare(blobs)); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	if _, err := db.CreateVersion(ctx, name, 2, "v2", actor, variantPrepare(blobs)); !errors.Is(err, store.ErrNoChanges) {
		t.Fatalf("second CreateVersion = %v, want ErrNoChanges", err)
	}
	if _, err := db.CreateVersion(ctx, name, 1, "old", actor, variantPrepare(blobs)); !errors.Is(err, store.ErrStaleRevision) {
		t.Errorf("old revision = %v, want ErrStaleRevision", err)
	}
}

// TestCreateVersionPrepareErrorAborts proves the transaction rolls back entirely when
// prepare fails: no version row and no audit row survive.
func TestCreateVersionPrepareErrorAborts(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.version_prep")
	if _, err := db.SaveDraft(ctx, name, []byte(`{"level":1}`), 1, actor); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	boom := errors.New("prepare exploded")
	_, err := db.CreateVersion(ctx, name, 2, "nope", actor,
		func([]byte, int, []byte) ([]byte, string, error) { return nil, "", boom })
	if !errors.Is(err, boom) {
		t.Fatalf("CreateVersion = %v, want the prepare error", err)
	}
	if n := countVersions(t, db, name); n != 0 {
		t.Errorf("%d version rows after a prepare error, want 0", n)
	}
	if n := countVersionAudit(t, db, name); n != 0 {
		t.Errorf("%d version.create audit rows after a prepare error, want 0", n)
	}
}

// TestCreateVersionUnknownNamespace checks the draft lock's missing-row path.
func TestCreateVersionUnknownNamespace(t *testing.T) {
	db := testDB(t)
	name := uniqueName("test.version_missing")
	_, err := db.CreateVersion(context.Background(), name, 1, "m", store.Entry{ActorID: "s", ActorName: "n"},
		func([]byte, int, []byte) ([]byte, string, error) { return []byte(`{}`), "x", nil })
	if !errors.Is(err, store.ErrNamespaceNotFound) {
		t.Errorf("CreateVersion = %v, want ErrNamespaceNotFound", err)
	}
}

// TestCreateVersionIsSerialised is the concurrency contract: five callers presenting
// the same revision must produce exactly one version; the rest see the winner's hash
// and get ErrNoChanges.
func TestCreateVersionIsSerialised(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.version_race")
	draft := []byte(`{"level":7}`)
	if _, err := db.SaveDraft(ctx, name, draft, 1, actor); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	const writers = 5
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = db.CreateVersion(ctx, name, 2, fmt.Sprintf("writer %d", i), actor, variantPrepare(blobs))
		}(i)
	}
	wg.Wait()

	var wins, noChanges int
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrNoChanges):
			noChanges++
		default:
			t.Errorf("concurrent CreateVersion %d: %v", i, err)
		}
	}
	if wins != 1 || noChanges != writers-1 {
		t.Errorf("outcomes = %d wins / %d no-changes, want 1 / %d", wins, noChanges, writers-1)
	}

	rows, err := db.Pool.Query(ctx, `SELECT version FROM config_version WHERE namespace = $1 ORDER BY version`, name)
	if err != nil {
		t.Fatalf("read versions: %v", err)
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
	if len(versions) != 1 || versions[0] != 1 {
		t.Errorf("versions = %v, want exactly [1]", versions)
	}
}

// versionsOf reduces a page to the version numbers in order, which is what the paging
// assertions are about.
func versionsOf(list []store.VersionSummary) []int {
	got := make([]int, 0, len(list))
	for _, v := range list {
		got = append(got, v.Version)
	}
	return got
}

// TestListVersionsAndGetVersion covers the read side of the version history: newest
// first, cursor paging, and a single version's body.
func TestListVersionsAndGetVersion(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.version_read")

	if _, err := db.ReplaceSchema(ctx, name, []byte(`{"type":"object"}`), nil, actor); err != nil {
		t.Fatalf("ReplaceSchema: %v", err)
	}
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	seed := func(revision int, body string) {
		t.Helper()
		if _, err := db.SaveDraft(ctx, name, []byte(body), revision, actor); err != nil {
			t.Fatalf("SaveDraft(%d): %v", revision, err)
		}
		if _, err := db.CreateVersion(ctx, name, revision+1, fmt.Sprintf("v%d", revision), actor, variantPrepare(blobs)); err != nil {
			t.Fatalf("CreateVersion(%d): %v", revision, err)
		}
	}
	seed(1, `{"level":1}`)
	seed(2, `{"level":2}`)
	seed(3, `{"level":3}`)

	list, next, err := db.ListVersions(ctx, name, nil, 50)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if got := versionsOf(list); !reflect.DeepEqual(got, []int{3, 2, 1}) {
		t.Errorf("versions = %v, want [3 2 1]", got)
	}
	if next != nil {
		t.Errorf("next = %d, want nil", *next)
	}
	if list[0].SchemaVersion != 2 || list[0].Message != "v3" {
		t.Errorf("newest = schema v%d %q, want schema v2 v3", list[0].SchemaVersion, list[0].Message)
	}

	list, next, err = db.ListVersions(ctx, name, nil, 2)
	if err != nil {
		t.Fatalf("ListVersions limit 2: %v", err)
	}
	if got := versionsOf(list); !reflect.DeepEqual(got, []int{3, 2}) {
		t.Errorf("limit 2 versions = %v, want [3 2]", got)
	}
	if next == nil || *next != 2 {
		t.Errorf("limit 2 next = %v, want 2", next)
	}

	before := 2
	list, next, err = db.ListVersions(ctx, name, &before, 50)
	if err != nil {
		t.Fatalf("ListVersions before 2: %v", err)
	}
	if got := versionsOf(list); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("before 2 versions = %v, want [1]", got)
	}
	if next != nil {
		t.Errorf("before 2 next = %d, want nil", *next)
	}

	v, err := db.GetVersion(ctx, name, 2)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if v.Version != 2 || v.SchemaVersion != 2 || v.Message != "v2" {
		t.Errorf("GetVersion = v%d schema v%d %q, want v2 schema v2 v2", v.Version, v.SchemaVersion, v.Message)
	}
	canonical, err := schema.Canonical(v.Body)
	if err != nil {
		t.Fatalf("Canonical(stored body): %v", err)
	}
	if want := `{"level":2}`; string(canonical) != want {
		t.Errorf("canonical body = %s, want %s", canonical, want)
	}

	if _, err := db.GetVersion(ctx, name, 99); !errors.Is(err, store.ErrVersionNotFound) {
		t.Errorf("GetVersion unknown = %v, want ErrVersionNotFound", err)
	}
	if _, _, err := db.ListVersions(ctx, uniqueName("test.version_nope"), nil, 50); !errors.Is(err, store.ErrNamespaceNotFound) {
		t.Errorf("ListVersions unknown ns = %v, want ErrNamespaceNotFound", err)
	}
}
