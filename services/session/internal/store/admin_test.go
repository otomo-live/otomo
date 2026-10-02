package store

import (
	"encoding/json"
	"errors"
	"testing"

	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

var staff = Entry{ActorID: "staff-7", ActorName: "Grace Hopper"}

// auditFor returns the audit rows whose target is target, newest first.
func auditFor(t *testing.T, db *DB, target string) []AuditRecord {
	t.Helper()
	all, _, err := db.ListAudit(t.Context(), AuditQuery{}, 200)
	if err != nil {
		t.Fatal(err)
	}
	var out []AuditRecord
	for _, e := range all {
		if e.Target == target {
			out = append(out, e)
		}
	}
	return out
}

// TestForceDisbandDuringAMatchIsAuditedAndTheCallbackFindsNothing is SE-7's criterion in
// the store, and doc 14 §2 note 2: staff may disband an in_game party, the audit row
// commits with the delete, every member is told, and the Allocator's later callback
// finds no party.
func TestForceDisbandDuringAMatchIsAuditedAndTheCallbackFindsNothing(t *testing.T) {
	db := testDB(t)
	leader, member, alloc, p := inGameLobby(t, db)

	actor := staff
	actor.Details = map[string]any{"reason": "griefing report 812"}
	gone, notices, err := db.ForceDisband(t.Context(), p.ID, actor)
	if err != nil || gone.ID != p.ID {
		t.Fatalf("ForceDisband = %+v, %v", gone, err)
	}
	told := noticesOf(notices, events.TypePartyDisbanded)
	if !told[leader] || !told[member] {
		t.Errorf("party.disbanded went to %v, want both", told)
	}
	if _, err := db.GetParty(t.Context(), leader); !errors.Is(err, ErrNotInParty) {
		t.Errorf("leader after the disband = %v, want ErrNotInParty", err)
	}

	rows := auditFor(t, db, p.ID)
	if len(rows) != 1 || rows[0].Action != ActionDisbandForced || rows[0].ActorID != "staff-7" || rows[0].ActorName != "Grace Hopper" {
		t.Fatalf("audit rows = %+v", rows)
	}
	var details map[string]any
	if err := json.Unmarshal(rows[0].Details, &details); err != nil {
		t.Fatal(err)
	}
	if details["state"] != StateInGame || details["allocation_id"] != alloc || details["reason"] != "griefing report 812" {
		t.Errorf("details = %v", details)
	}

	if _, _, done, err := db.ReturnFromGame(t.Context(), p.ID, alloc, "ended"); err != nil || done {
		t.Errorf("callback after the disband = %v %v, want nothing done", done, err)
	}
	if _, _, err := db.ForceDisband(t.Context(), p.ID, staff); !errors.Is(err, ErrPartyNotFound) {
		t.Errorf("second disband = %v, want ErrPartyNotFound", err)
	}
}

// TestADisbandWhoseAuditFailsDeletesNothing: the audit row and the delete are one
// transaction (SES-A5).
func TestADisbandWhoseAuditFailsDeletesNothing(t *testing.T) {
	db := testDB(t)
	leader := player(t, db)
	p, _, err := db.CreateParty(t.Context(), leader, 4, seedSettings)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ForceDisband(t.Context(), p.ID, Entry{ActorID: "staff-7"}); err == nil {
		t.Fatal("a disband with no actor name was accepted")
	}
	if _, err := db.GetParty(t.Context(), leader); err != nil {
		t.Errorf("the party is gone after a failed audit: %v", err)
	}
	if rows := auditFor(t, db, p.ID); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none", rows)
	}
}

func TestListAuditPagesNewestFirst(t *testing.T) {
	db := testDB(t)
	var targets []string
	for range 3 {
		leader := player(t, db)
		p, _, err := db.CreateParty(t.Context(), leader, 4, seedSettings)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.ForceDisband(t.Context(), p.ID, staff); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, p.ID)
	}
	actor := "Grace Hopper"
	first, next, err := db.ListAudit(t.Context(), AuditQuery{Actor: &actor}, 2)
	if err != nil || len(first) != 2 || next == nil {
		t.Fatalf("first page = %d rows, next %v, %v", len(first), next, err)
	}
	if first[0].Target != targets[2] || first[1].Target != targets[1] || first[0].ID <= first[1].ID {
		t.Errorf("first page is not newest first: %+v", first)
	}
	second, _, err := db.ListAudit(t.Context(), AuditQuery{Actor: &actor, Before: next}, 2)
	if err != nil || len(second) == 0 || second[0].Target != targets[0] {
		t.Errorf("second page starts with %+v, %v; want the oldest disband", second, err)
	}
}

func TestLookupPlayers(t *testing.T) {
	db := testDB(t)
	p := player(t, db)
	var name string
	if err := db.Pool.QueryRow(t.Context(), `SELECT display_name FROM player_profile WHERE player_id = $1`, p).Scan(&name); err != nil {
		t.Fatal(err)
	}
	one := 1
	found, err := db.LookupPlayers(t.Context(), name, &one)
	if err != nil || len(found) != 1 || found[0].PlayerID != uuid.MustParse(p) {
		t.Errorf("LookupPlayers(%s#1) = %+v, %v", name, found, err)
	}
	two := 2
	if found, err := db.LookupPlayers(t.Context(), name, &two); err != nil || len(found) != 0 {
		t.Errorf("wrong discriminator = %+v, %v", found, err)
	}
}
