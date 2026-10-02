package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/store"
)

// These exercise create-and-list against a real Postgres, because the transaction,
// the unique violation and the audit row are all properties of the database. Names
// are suffixed per run: other tests share the database, so a fixed name would collide
// with whatever a previous run left behind.

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

// TestCreateNamespaceThenList creates a namespace and reads it back through the same
// query the API list uses, asserting the empty-draft shape and that the audit row
// landed in the transaction.
func TestCreateNamespaceThenList(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := uniqueName("test.ns")
	actor := store.Entry{ActorID: uniqueName("staff"), ActorName: "Namespace Tester"}

	created, err := db.CreateNamespace(ctx, name, "client", "", actor)
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if created.Name != name || created.Audience != "client" {
		t.Errorf("created = %+v, want name %q audience client", created, name)
	}
	if created.Description != "" {
		t.Errorf("Description = %q, want empty", created.Description)
	}
	if created.LatestVersion != nil {
		t.Errorf("LatestVersion = %v, want nil", *created.LatestVersion)
	}
	if created.Draft.Revision != 1 {
		t.Errorf("Revision = %d, want 1", created.Draft.Revision)
	}
	if created.Draft.HasUnpublishedChanges {
		t.Error("HasUnpublishedChanges = true, want false for a fresh namespace")
	}
	if created.CreatedAt.IsZero() || created.Draft.UpdatedAt.IsZero() {
		t.Errorf("timestamps are zero: created_at=%v updated_at=%v", created.CreatedAt, created.Draft.UpdatedAt)
	}

	var descIsNull bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT description IS NULL FROM config_namespace WHERE name = $1`, name).Scan(&descIsNull); err != nil {
		t.Fatalf("read description: %v", err)
	}
	if !descIsNull {
		t.Error("empty description was stored as a non-NULL value")
	}

	list, err := db.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	got := findNamespace(list, name)
	if got == nil {
		t.Fatalf("ListNamespaces did not return %q", name)
	}
	if got.Description != "" || got.LatestVersion != nil || got.Draft.Revision != 1 || got.Draft.HasUnpublishedChanges {
		t.Errorf("listed = %+v, want the freshly created shape", got)
	}

	if n := countNamespaceAudit(t, db, ctx, name); n != 1 {
		t.Errorf("%d namespace.create audit rows for %q, want 1", n, name)
	}
}

// TestCreateNamespaceStoresDescription checks the other branch of the NULL rule.
func TestCreateNamespaceStoresDescription(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := uniqueName("test.desc")
	actor := store.Entry{ActorID: uniqueName("staff"), ActorName: "Namespace Tester"}

	if _, err := db.CreateNamespace(ctx, name, "server", "the gameplay namespace", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	list, err := db.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	got := findNamespace(list, name)
	if got == nil {
		t.Fatalf("ListNamespaces did not return %q", name)
	}
	if got.Description != "the gameplay namespace" {
		t.Errorf("Description = %q, want %q", got.Description, "the gameplay namespace")
	}
}

// TestCreateNamespaceDuplicate checks the sentinel and, critically, that the failed
// attempt wrote no second audit row: the whole transaction rolls back.
func TestCreateNamespaceDuplicate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := uniqueName("test.dup")
	actor := store.Entry{ActorID: uniqueName("staff"), ActorName: "Namespace Tester"}

	if _, err := db.CreateNamespace(ctx, name, "client", "", actor); err != nil {
		t.Fatalf("first CreateNamespace: %v", err)
	}

	_, err := db.CreateNamespace(ctx, name, "client", "", actor)
	if !errors.Is(err, store.ErrNamespaceExists) {
		t.Fatalf("second CreateNamespace error = %v, want ErrNamespaceExists", err)
	}

	if n := countNamespaceAudit(t, db, ctx, name); n != 1 {
		t.Errorf("%d namespace.create audit rows after a duplicate, want 1", n)
	}
}

func findNamespace(list []store.Namespace, name string) *store.Namespace {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

func countNamespaceAudit(t *testing.T, db *store.DB, ctx context.Context, target string) int {
	t.Helper()

	var n int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'namespace.create' AND target = $1`, target).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}
