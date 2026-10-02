package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/store"
)

// TestListAuditFiltersAndPaging writes a known set of entries and reads them back
// through the same filters the audit route exposes.
func TestListAuditFiltersAndPaging(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	actorID := uniqueName("test.audit_actor")
	actorName := uniqueName("Audit Actor")
	otherName := uniqueName("Other Actor")
	action := uniqueName("test.audit.action")

	start := time.Now().Add(-time.Second)
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	entries := []store.Entry{
		{ActorID: actorID, ActorName: actorName, Action: action, Target: "one"},
		{ActorID: actorID, ActorName: actorName, Action: action, Target: "two"},
		{ActorID: actorID, ActorName: actorName, Action: action, Target: "three"},
		{ActorID: uniqueName("test.audit_other"), ActorName: otherName, Action: action, Target: "other"},
	}
	for _, e := range entries {
		if err := store.WriteAudit(ctx, tx, e); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("WriteAudit: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	end := time.Now().Add(time.Second)

	// action is exact and actor matches actor_id.
	page, next, err := db.ListAudit(ctx, store.AuditQuery{Action: &action, Actor: &actorID}, 2)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("page size = %d, want 2", len(page))
	}
	if page[0].Target != "three" || page[1].Target != "two" {
		t.Errorf("targets = %q,%q, want three,two", page[0].Target, page[1].Target)
	}
	if next == nil || *next != page[1].ID {
		t.Fatalf("next = %v, want %d", next, page[1].ID)
	}
	if page[0].ActorID != actorID || page[0].ActorName != actorName || page[0].Action != action {
		t.Errorf("entry = %+v", page[0])
	}

	rest, next2, err := db.ListAudit(ctx, store.AuditQuery{Action: &action, Actor: &actorID, Before: next}, 2)
	if err != nil {
		t.Fatalf("ListAudit(page 2): %v", err)
	}
	if len(rest) != 1 || rest[0].Target != "one" {
		t.Fatalf("page 2 = %+v, want just target one", rest)
	}
	if next2 != nil {
		t.Errorf("page 2 next = %v, want nil", next2)
	}

	// actor also matches actor_name.
	byName, _, err := db.ListAudit(ctx, store.AuditQuery{Action: &action, Actor: &otherName}, 10)
	if err != nil {
		t.Fatalf("ListAudit(by name): %v", err)
	}
	if len(byName) != 1 || byName[0].Target != "other" {
		t.Errorf("by name = %+v, want just target other", byName)
	}

	// from/to bound `at`.
	window, _, err := db.ListAudit(ctx, store.AuditQuery{Action: &action, From: &start, To: &end}, 10)
	if err != nil {
		t.Fatalf("ListAudit(window): %v", err)
	}
	if len(window) != len(entries) {
		t.Errorf("window returned %d entries, want %d", len(window), len(entries))
	}
	future := end.Add(time.Hour)
	if none, _, err := db.ListAudit(ctx, store.AuditQuery{Action: &action, From: &future}, 10); err != nil {
		t.Fatalf("ListAudit(future): %v", err)
	} else if len(none) != 0 {
		t.Errorf("future window returned %d entries, want 0", len(none))
	}

	// The raw details body is JSON, so the API can embed it as an object.
	if string(page[0].Details) != "{}" {
		t.Errorf("details = %s, want {}", page[0].Details)
	}
}
