package pool

import "testing"

// TestExpireReservationsExpiresAndFreesServer is the happy path: an expired reservation
// becomes expired with its reason and outbox flag, and the server it held is freed.
func TestExpireReservationsExpiresAndFreesServer(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	alloc := insertAllocation(t, db, serverAlpha, "reserved", []string{playerA})
	assignServer(t, db, serverAlpha, alloc, "reserved")

	if _, err := db.Exec(t.Context(), `
		UPDATE allocation SET expires_at = now() - interval '1 minute'
		WHERE allocation_id = $1::text::uuid`, alloc); err != nil {
		t.Fatalf("expire allocation: %v", err)
	}

	n, err := p.ExpireReservations(t.Context())
	if err != nil {
		t.Fatalf("ExpireReservations: %v", err)
	}
	if n != 1 {
		t.Fatalf("ExpireReservations expired %d allocations, want 1", n)
	}

	a := getAllocation(t, db, alloc)
	if a.status != "expired" {
		t.Errorf("allocation status = %q, want expired", a.status)
	}
	if a.endReason != "expired" {
		t.Errorf("end_reason = %q, want expired", a.endReason)
	}
	if !a.ended {
		t.Error("ended_at is null for an expired allocation")
	}
	if !a.callbackPending {
		t.Error("callback_pending is false; Session would never be told")
	}

	s := getServer(t, db, serverAlpha)
	if s.state != "free" || s.allocationID != "" || s.playersConnected != 0 {
		t.Errorf("server = %+v, want free with no allocation", s)
	}
}

// TestExpireReservationsLeavesUnexpiredReservation checks the boundary: a reservation
// still inside its TTL is untouched and is not counted.
func TestExpireReservationsLeavesUnexpiredReservation(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	alloc := insertAllocation(t, db, serverAlpha, "reserved", []string{playerA})
	assignServer(t, db, serverAlpha, alloc, "reserved")

	n, err := p.ExpireReservations(t.Context())
	if err != nil {
		t.Fatalf("ExpireReservations: %v", err)
	}
	if n != 0 {
		t.Fatalf("ExpireReservations expired %d allocations, want 0", n)
	}

	if a := getAllocation(t, db, alloc); a.status != "reserved" {
		t.Errorf("allocation status = %q, want reserved", a.status)
	}
	if s := getServer(t, db, serverAlpha); s.state != "reserved" || s.allocationID != alloc {
		t.Errorf("server = %+v, want still reserved holding %q", s, alloc)
	}
}

// TestExpireReservationsLeavesActiveAllocation checks that the status filter matters: an
// active allocation whose expires_at has passed is the party's, not a stale hold, so the
// reaper path (a missing heartbeat) is what ends it, not expiry.
func TestExpireReservationsLeavesActiveAllocation(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	alloc := insertAllocation(t, db, serverAlpha, "active", []string{playerA})
	assignServer(t, db, serverAlpha, alloc, "busy")

	if _, err := db.Exec(t.Context(), `
		UPDATE allocation SET expires_at = now() - interval '1 minute'
		WHERE allocation_id = $1::text::uuid`, alloc); err != nil {
		t.Fatalf("expire allocation: %v", err)
	}

	n, err := p.ExpireReservations(t.Context())
	if err != nil {
		t.Fatalf("ExpireReservations: %v", err)
	}
	if n != 0 {
		t.Fatalf("ExpireReservations expired %d allocations, want 0", n)
	}

	if a := getAllocation(t, db, alloc); a.status != "active" {
		t.Errorf("allocation status = %q, want active", a.status)
	}
	if s := getServer(t, db, serverAlpha); s.state != "busy" || s.allocationID != alloc {
		t.Errorf("server = %+v, want still busy holding %q", s, alloc)
	}
}

// TestExpireReservationsDoesNotFreeReassignedServer covers the guard on the server
// update. The server re-registered after the expired reservation and now points at a
// different allocation; expiring the old one must not free a server that has moved on.
func TestExpireReservationsDoesNotFreeReassignedServer(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)

	stale := insertAllocation(t, db, serverAlpha, "reserved", []string{playerA})
	if _, err := db.Exec(t.Context(), `
		UPDATE allocation SET expires_at = now() - interval '1 minute'
		WHERE allocation_id = $1::text::uuid`, stale); err != nil {
		t.Fatalf("expire allocation: %v", err)
	}
	current := insertAllocation(t, db, serverAlpha, "reserved", []string{playerB})
	assignServer(t, db, serverAlpha, current, "reserved")

	n, err := p.ExpireReservations(t.Context())
	if err != nil {
		t.Fatalf("ExpireReservations: %v", err)
	}
	if n != 1 {
		t.Fatalf("ExpireReservations expired %d allocations, want 1", n)
	}

	if a := getAllocation(t, db, stale); a.status != "expired" {
		t.Errorf("expired allocation status = %q, want expired", a.status)
	}
	if a := getAllocation(t, db, current); a.status != "reserved" {
		t.Errorf("current allocation status = %q, want still reserved", a.status)
	}
	if s := getServer(t, db, serverAlpha); s.state != "reserved" || s.allocationID != current {
		t.Errorf("server = %+v, want reserved holding the current allocation %q", s, current)
	}
}
