package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/otomo-live/otomo/services/config/internal/store"
)

// createTestNamespace makes a fresh namespace for a schema test and returns its name.
func createTestNamespace(t *testing.T, db *store.DB, prefix string) (string, store.Entry) {
	t.Helper()

	actor := store.Entry{ActorID: uniqueName("staff"), ActorName: "Schema Tester"}
	name := uniqueName(prefix)
	if _, err := db.CreateNamespace(context.Background(), name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	return name, actor
}

// TestLatestSchemaAndReplace covers the create/read/replace cycle: a new namespace
// starts at v1 with an empty object, a replace appends v2 and leaves exactly one audit
// row whose details name the new version.
func TestLatestSchemaAndReplace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.schema")

	initial, err := db.LatestSchema(ctx, name)
	if err != nil {
		t.Fatalf("LatestSchema on a new namespace: %v", err)
	}
	if initial.Namespace != name || initial.SchemaVersion != 1 {
		t.Errorf("initial = %s v%d, want %s v1", initial.Namespace, initial.SchemaVersion, name)
	}
	if string(initial.Body) != "{}" {
		t.Errorf("initial body = %s, want {}", initial.Body)
	}
	if initial.CreatedBy != actor.ActorID {
		t.Errorf("initial CreatedBy = %q, want %q", initial.CreatedBy, actor.ActorID)
	}

	body := []byte(`{"type":"object","properties":{"level":{"type":"integer"}}}`)
	expected := 1
	replaced, err := db.ReplaceSchema(ctx, name, body, &expected, actor)
	if err != nil {
		t.Fatalf("ReplaceSchema: %v", err)
	}
	if replaced.Namespace != name || replaced.SchemaVersion != 2 {
		t.Errorf("replaced = %s v%d, want %s v2", replaced.Namespace, replaced.SchemaVersion, name)
	}
	if replaced.CreatedBy != actor.ActorID {
		t.Errorf("replaced CreatedBy = %q, want %q", replaced.CreatedBy, actor.ActorID)
	}
	assertSameJSON(t, replaced.Body, body)

	latest, err := db.LatestSchema(ctx, name)
	if err != nil {
		t.Fatalf("LatestSchema after replace: %v", err)
	}
	if latest.SchemaVersion != 2 {
		t.Errorf("latest schema_version = %d, want 2", latest.SchemaVersion)
	}
	assertSameJSON(t, latest.Body, body)

	var count int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'schema.replace' AND target = $1`, name).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("%d schema.replace audit rows, want 1", count)
	}

	var details []byte
	if err := db.Pool.QueryRow(ctx,
		`SELECT details FROM audit_log WHERE action = 'schema.replace' AND target = $1`, name).Scan(&details); err != nil {
		t.Fatalf("read audit details: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(details, &got); err != nil {
		t.Fatalf("audit details are not JSON: %v", err)
	}
	if got["schema_version"] != float64(2) {
		t.Errorf("audit details = %v, want schema_version 2", got)
	}
}

// TestSchemaUnknownNamespace checks both accessors against a name that was never
// created.
func TestSchemaUnknownNamespace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name := uniqueName("test.missing")

	if _, err := db.LatestSchema(ctx, name); !errors.Is(err, store.ErrNamespaceNotFound) {
		t.Errorf("LatestSchema error = %v, want ErrNamespaceNotFound", err)
	}
	_, err := db.ReplaceSchema(ctx, name, []byte(`{}`), nil, store.Entry{ActorID: "s", ActorName: "n"})
	if !errors.Is(err, store.ErrNamespaceNotFound) {
		t.Errorf("ReplaceSchema error = %v, want ErrNamespaceNotFound", err)
	}
}

// TestReplaceSchemaStaleWritesNothing proves the optimistic guard: a replace whose
// expected version is no longer current returns ErrStaleSchema carrying the current
// schema, and leaves the row and the audit log untouched.
func TestReplaceSchemaStaleWritesNothing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.stale_schema")

	first := 1
	if _, err := db.ReplaceSchema(ctx, name, []byte(`{"type":"object"}`), &first, actor); err != nil {
		t.Fatalf("ReplaceSchema: %v", err)
	}

	// expected is now one behind: the version the caller edited was consumed.
	_, err := db.ReplaceSchema(ctx, name, []byte(`{"type":"object","title":"loser"}`), &first, actor)
	if !errors.Is(err, store.ErrStaleSchema) {
		t.Fatalf("stale ReplaceSchema = %v, want ErrStaleSchema", err)
	}
	var stale *store.StaleSchemaError
	if !errors.As(err, &stale) {
		t.Fatalf("stale ReplaceSchema = %v, want *StaleSchemaError", err)
	}
	if stale.Current.SchemaVersion != 2 {
		t.Errorf("stale Current.SchemaVersion = %d, want 2", stale.Current.SchemaVersion)
	}

	latest, err := db.LatestSchema(ctx, name)
	if err != nil {
		t.Fatalf("LatestSchema: %v", err)
	}
	if latest.SchemaVersion != 2 {
		t.Errorf("latest schema_version = %d, want 2 (the stale write must not land)", latest.SchemaVersion)
	}
	assertSameJSON(t, latest.Body, []byte(`{"type":"object"}`))

	var count int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'schema.replace' AND target = $1`, name).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if count != 1 {
		t.Errorf("%d schema.replace audit rows after the stale write, want 1", count)
	}

	// nil means "no check", for internal callers that just observed the version.
	third, err := db.ReplaceSchema(ctx, name, []byte(`{"type":"object","title":"unchecked"}`), nil, actor)
	if err != nil {
		t.Fatalf("unchecked ReplaceSchema: %v", err)
	}
	if third.SchemaVersion != 3 {
		t.Errorf("unchecked ReplaceSchema version = %d, want 3", third.SchemaVersion)
	}
}

// TestReplaceSchemaIsSerialised is the row lock's acceptance test: ten concurrent
// replaces, each holding the version it just read and retrying on a stale refusal, must
// produce ten distinct, consecutive versions rather than racing for the same primary
// key or losing a write.
func TestReplaceSchemaIsSerialised(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.concurrent")

	const writers = 10
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(fmt.Sprintf(`{"writer":%d}`, i))
			for {
				current, err := db.LatestSchema(ctx, name)
				if err != nil {
					errs[i] = err
					return
				}
				expected := current.SchemaVersion
				if _, err := db.ReplaceSchema(ctx, name, body, &expected, actor); err == nil {
					return
				} else if !errors.Is(err, store.ErrStaleSchema) {
					errs[i] = err
					return
				}
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent ReplaceSchema %d: %v", i, err)
		}
	}

	rows, err := db.Pool.Query(ctx,
		`SELECT schema_version FROM config_schema WHERE namespace = $1 ORDER BY schema_version`, name)
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := make([]int, writers+1)
	for i := range want {
		want[i] = i + 1
	}
	if !reflect.DeepEqual(versions, want) {
		t.Errorf("versions = %v, want %v", versions, want)
	}
}

// assertSameJSON compares two JSON documents by value, since jsonb does not preserve
// key order or whitespace.
func assertSameJSON(t *testing.T, got, want []byte) {
	t.Helper()

	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want is not JSON: %v (%s)", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("body = %s, want %s", got, want)
	}
}
