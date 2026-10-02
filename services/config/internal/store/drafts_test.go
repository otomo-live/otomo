package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/otomo-live/otomo/services/config/internal/store"
)

// TestDraftAndSaveDraft covers the read/save cycle: a fresh namespace drafts revision 1
// with an empty object and no base version, a save at 1 advances to revision 2 and
// leaves exactly one audit row, and the same save again is stale and writes nothing.
func TestDraftAndSaveDraft(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.draft")

	fresh, err := db.Draft(ctx, name)
	if err != nil {
		t.Fatalf("Draft on a new namespace: %v", err)
	}
	if fresh.Namespace != name || fresh.Revision != 1 {
		t.Errorf("fresh = %s r%d, want %s r1", fresh.Namespace, fresh.Revision, name)
	}
	if string(fresh.Body) != "{}" {
		t.Errorf("fresh body = %s, want {}", fresh.Body)
	}
	if fresh.BaseVersion != nil {
		t.Errorf("fresh base_version = %d, want nil", *fresh.BaseVersion)
	}
	if fresh.UpdatedBy != actor.ActorID {
		t.Errorf("fresh UpdatedBy = %q, want %q", fresh.UpdatedBy, actor.ActorID)
	}

	body := []byte(`{"level":3,"title":"start"}`)
	saved, err := db.SaveDraft(ctx, name, body, 1, actor)
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if saved.Revision != 2 {
		t.Errorf("saved revision = %d, want 2", saved.Revision)
	}

	after, err := db.Draft(ctx, name)
	if err != nil {
		t.Fatalf("Draft after save: %v", err)
	}
	if after.Revision != 2 {
		t.Errorf("revision after save = %d, want 2", after.Revision)
	}
	assertSameJSON(t, after.Body, body)
	if after.UpdatedBy != actor.ActorID {
		t.Errorf("UpdatedBy after save = %q, want %q", after.UpdatedBy, actor.ActorID)
	}

	if n := countDraftAudit(t, db, ctx, name); n != 1 {
		t.Fatalf("%d draft.save audit rows after save, want 1", n)
	}

	if _, err := db.SaveDraft(ctx, name, []byte(`{"level":4}`), 1, actor); !errors.Is(err, store.ErrStaleRevision) {
		t.Errorf("SaveDraft at a consumed revision = %v, want ErrStaleRevision", err)
	}
	if n := countDraftAudit(t, db, ctx, name); n != 1 {
		t.Errorf("%d draft.save audit rows after a stale save, want 1", n)
	}
}

// TestDraftUnknownNamespace checks both accessors against a name that was never
// created.
func TestDraftUnknownNamespace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name := uniqueName("test.draft_missing")

	if _, err := db.Draft(ctx, name); !errors.Is(err, store.ErrNamespaceNotFound) {
		t.Errorf("Draft error = %v, want ErrNamespaceNotFound", err)
	}
	_, err := db.SaveDraft(ctx, name, []byte(`{}`), 1, store.Entry{ActorID: "s", ActorName: "n"})
	if !errors.Is(err, store.ErrNamespaceNotFound) {
		t.Errorf("SaveDraft error = %v, want ErrNamespaceNotFound", err)
	}
}

// TestSaveDraftIsOptimistic is the revision guard's acceptance test: ten concurrent
// saves at the same revision must produce exactly one winner. The database's row lock
// is what serialises them; the WHERE revision clause is what makes the nine losers
// stale rather than silently overwriting the winner.
func TestSaveDraftIsOptimistic(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.draft_race")

	const writers = 10
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = db.SaveDraft(ctx, name, []byte(`{"winner":1}`), 1, actor)
		}(i)
	}
	wg.Wait()

	var wins, stales int
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrStaleRevision):
			stales++
		default:
			t.Errorf("concurrent SaveDraft %d: %v", i, err)
		}
	}
	if wins != 1 || stales != writers-1 {
		t.Errorf("outcomes = %d wins / %d stale, want 1 / %d", wins, stales, writers-1)
	}

	final, err := db.Draft(ctx, name)
	if err != nil {
		t.Fatalf("Draft after the race: %v", err)
	}
	if final.Revision != 2 {
		t.Errorf("final revision = %d, want 2", final.Revision)
	}
	if n := countDraftAudit(t, db, ctx, name); n != 1 {
		t.Errorf("%d draft.save audit rows after the race, want 1", n)
	}
}

// countDraftAudit counts draft.save rows for one namespace, so a stale save can be
// proven to have written nothing.
func countDraftAudit(t *testing.T, db *store.DB, ctx context.Context, name string) int {
	t.Helper()

	var n int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'draft.save' AND target = $1`, name).Scan(&n); err != nil {
		t.Fatalf("count draft.save audit rows: %v", err)
	}
	return n
}
