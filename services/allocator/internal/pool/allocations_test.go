package pool

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// A third server and a third player, plus two fixed parties. The pool tests already
// define serverAlpha, serverBeta, playerA and playerB.
const (
	serverGamma = "gs-gamma"

	partyOne = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	partyTwo = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	playerC = "33333333-3333-3333-3333-333333333333"
)

// mustRegisterCapacity registers a fresh, heartbeating server with a specific
// capacity. Registration sets last_heartbeat, so the row is eligible immediately.
func mustRegisterCapacity(t *testing.T, p *Pool, serverID string, capacity int) {
	t.Helper()
	reg := Registration{ServerID: serverID, InternalAddr: "10.0.0.1:27015", Capacity: capacity}
	if err := p.Register(t.Context(), reg); err != nil {
		t.Fatalf("Register %q: %v", serverID, err)
	}
}

// TestValidateAllocationRequest pins the wire rules and the normalisation without a
// database, so a rejection is attributed to the field the caller has to fix.
func TestValidateAllocationRequest(t *testing.T) {
	got, err := ValidateAllocationRequest(partyOne, []string{strings.ToUpper(playerB), playerA})
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if want := []string{playerA, playerB}; !slices.Equal(got, want) {
		t.Errorf("normalised players = %v, want %v", got, want)
	}

	cases := []struct {
		name    string
		partyID string
		players []string
		field   string
	}{
		{"bad party id", "not-a-uuid", []string{playerA}, "party_id"},
		{"no players", partyOne, nil, "player_ids"},
		{"too many players", partyOne, []string{
			"00000000-0000-0000-0000-000000000001",
			"00000000-0000-0000-0000-000000000002",
			"00000000-0000-0000-0000-000000000003",
			"00000000-0000-0000-0000-000000000004",
			"00000000-0000-0000-0000-000000000005",
			"00000000-0000-0000-0000-000000000006",
			"00000000-0000-0000-0000-000000000007",
			"00000000-0000-0000-0000-000000000008",
			"00000000-0000-0000-0000-000000000009",
		}, "player_ids"},
		{"bad player id", partyOne, []string{"not-a-uuid"}, "player_ids"},
		{"duplicate players", partyOne, []string{strings.ToUpper(playerA), playerA}, "player_ids"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateAllocationRequest(tc.partyID, tc.players)
			if err == nil {
				t.Fatalf("ValidateAllocationRequest(%q, %v) accepted an invalid request", tc.partyID, tc.players)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error does not name %s: %v", tc.field, err)
			}
		})
	}
}

// TestAllocateReservesLowestFreeServer is the happy path: the lowest-ordered free
// server is taken, marked reserved, and pointed at the new allocation.
func TestAllocateReservesLowestFreeServer(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverBeta)
	mustRegister(t, p, serverAlpha)

	ctx := t.Context()
	alloc, created, err := p.Allocate(ctx, partyOne, []string{playerA, playerB})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if !created {
		t.Error("created = false, want true for a new allocation")
	}
	if alloc.AllocationID == "" {
		t.Error("allocation_id is empty")
	}
	if alloc.ServerID != serverAlpha {
		t.Errorf("server_id = %q, want the lowest free server %q", alloc.ServerID, serverAlpha)
	}
	if alloc.Status != "reserved" {
		t.Errorf("status = %q, want reserved", alloc.Status)
	}
	if alloc.ExpiresAt.IsZero() {
		t.Error("expires_at is the zero time")
	}

	// The reservation TTL is decided by Postgres's clock, so ask Postgres whether the
	// stored value is in its own future rather than comparing against the Go clock.
	var future bool
	if err := db.QueryRow(ctx, `
		SELECT expires_at > now() FROM allocation WHERE allocation_id = $1::text::uuid`,
		alloc.AllocationID).Scan(&future); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if !future {
		t.Error("expires_at is not in the database's future")
	}

	s := getServer(t, db, serverAlpha)
	if s.state != "reserved" || s.allocationID != alloc.AllocationID {
		t.Errorf("server = %+v, want reserved holding %q", s, alloc.AllocationID)
	}
	if other := getServer(t, db, serverBeta); other.state != "free" {
		t.Errorf("server %q state = %q, want still free", serverBeta, other.state)
	}
}

// TestAllocateIsIdempotentForSamePlayerSet covers the retry path and the set
// comparison: order and case differences are the same set, so the same allocation
// comes back with created=false.
func TestAllocateIsIdempotentForSamePlayerSet(t *testing.T) {
	p, _ := newPool(t)
	mustRegister(t, p, serverAlpha)

	ctx := t.Context()
	first, created, err := p.Allocate(ctx, partyOne, []string{playerA, playerB})
	if err != nil || !created {
		t.Fatalf("first Allocate = (%+v, %v, %v), want a new allocation", first, created, err)
	}

	second, created, err := p.Allocate(ctx, partyOne, []string{strings.ToUpper(playerB), playerA})
	if err != nil {
		t.Fatalf("retried Allocate: %v", err)
	}
	if created {
		t.Error("created = true on an idempotent retry")
	}
	if second.AllocationID != first.AllocationID {
		t.Errorf("allocation_id = %q, want the existing %q", second.AllocationID, first.AllocationID)
	}
	if second.ServerID != first.ServerID {
		t.Errorf("server_id = %q, want %q", second.ServerID, first.ServerID)
	}
}

// TestAllocateConflictsOnDifferentPlayerSet checks that a party id may not be reused
// for a different roster while it holds a live allocation.
func TestAllocateConflictsOnDifferentPlayerSet(t *testing.T) {
	p, _ := newPool(t)
	mustRegister(t, p, serverAlpha)

	ctx := t.Context()
	if _, _, err := p.Allocate(ctx, partyOne, []string{playerA, playerB}); err != nil {
		t.Fatalf("first Allocate: %v", err)
	}

	for _, players := range [][]string{{playerA}, {playerA, playerB, playerC}} {
		if _, _, err := p.Allocate(ctx, partyOne, players); !errors.Is(err, ErrAllocationConflict) {
			t.Errorf("Allocate for %v = %v, want ErrAllocationConflict", players, err)
		}
	}
}

func TestAllocateWithNoFreeServerIsNoCapacity(t *testing.T) {
	p, _ := newPool(t)

	if _, _, err := p.Allocate(t.Context(), partyOne, []string{playerA}); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Allocate with an empty pool = %v, want ErrNoCapacity", err)
	}
}

// TestAllocateSkipsStaleAndTooSmallServers checks both eligibility filters: a free
// server that cannot hold the party and one whose heartbeat is older than the timeout
// are passed over in favour of a fresh, large enough server.
func TestAllocateSkipsStaleAndTooSmallServers(t *testing.T) {
	p, db := newPool(t)
	mustRegisterCapacity(t, p, serverAlpha, 1)
	mustRegister(t, p, serverBeta)
	if _, err := db.Exec(t.Context(), `
		UPDATE game_server SET last_heartbeat = now() - interval '1 minute'
		WHERE server_id = $1`, serverBeta); err != nil {
		t.Fatalf("make %q stale: %v", serverBeta, err)
	}
	mustRegister(t, p, serverGamma)

	alloc, created, err := p.Allocate(t.Context(), partyOne, []string{playerA, playerB})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if !created {
		t.Error("created = false, want true")
	}
	if alloc.ServerID != serverGamma {
		t.Errorf("server_id = %q, want %q (alpha is too small, beta is stale)", alloc.ServerID, serverGamma)
	}
}

// TestAllocateAfterAllocationEnded checks that ending a hold frees both the server and
// the party: the unique live-party index is partial, so the next request is a new
// reservation rather than a conflict or an idempotent return.
func TestAllocateAfterAllocationEnded(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)

	ctx := t.Context()
	first, _, err := p.Allocate(ctx, partyOne, []string{playerA})
	if err != nil {
		t.Fatalf("first Allocate: %v", err)
	}
	if err := p.Ended(ctx, serverAlpha, first.AllocationID); err != nil {
		t.Fatalf("Ended: %v", err)
	}

	second, created, err := p.Allocate(ctx, partyOne, []string{playerA})
	if err != nil {
		t.Fatalf("second Allocate: %v", err)
	}
	if !created {
		t.Error("created = false after the previous allocation ended, want true")
	}
	if second.AllocationID == first.AllocationID {
		t.Error("the second allocation reused the ended one's id")
	}

	if a := getAllocation(t, db, first.AllocationID); a.status != "ended" {
		t.Errorf("first allocation status = %q, want ended", a.status)
	}
}

// TestAllocateConcurrency checks the SKIP LOCKED guarantee end to end: ten requests
// for ten different parties against three free servers yield exactly three
// allocations on three distinct servers, and the rest are honestly told there is no
// capacity rather than sharing a server.
func TestAllocateConcurrency(t *testing.T) {
	p, _ := newPool(t)
	mustRegister(t, p, serverAlpha)
	mustRegister(t, p, serverBeta)
	mustRegister(t, p, serverGamma)

	const requests = 10
	ctx := t.Context()

	serverIDs := make([]string, requests)
	createdFlags := make([]bool, requests)
	errs := make([]error, requests)

	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			partyID := fmt.Sprintf("00000000-0000-0000-0000-%012x", i)
			alloc, created, err := p.Allocate(ctx, partyID, []string{playerA})
			serverIDs[i] = alloc.ServerID
			createdFlags[i] = created
			errs[i] = err
		}(i)
	}
	wg.Wait()

	successes, noCapacity := 0, 0
	reserved := make(map[string]bool, 3)
	for i, err := range errs {
		switch {
		case err == nil:
			successes++
			if !createdFlags[i] {
				t.Errorf("request %d created = false, want true", i)
			}
			if reserved[serverIDs[i]] {
				t.Errorf("request %d reused server %q", i, serverIDs[i])
			}
			reserved[serverIDs[i]] = true
		case errors.Is(err, ErrNoCapacity):
			noCapacity++
		default:
			t.Errorf("request %d failed unexpectedly: %v", i, err)
		}
	}

	if successes != 3 {
		t.Errorf("successes = %d, want 3", successes)
	}
	if noCapacity != 7 {
		t.Errorf("no-capacity answers = %d, want 7", noCapacity)
	}
	if len(reserved) != 3 {
		t.Errorf("distinct servers = %d, want 3", len(reserved))
	}
}

// TestAllocateConcurrencySameParty checks the unique-index half of the guarantee:
// eight racing requests for one party all succeed with the same allocation, only one
// request created it, and exactly one server is reserved.
func TestAllocateConcurrencySameParty(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	mustRegister(t, p, serverBeta)
	mustRegister(t, p, serverGamma)

	const requests = 8
	ctx := t.Context()
	players := []string{playerA, playerB}

	ids := make([]string, requests)
	createdFlags := make([]bool, requests)
	errs := make([]error, requests)

	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			alloc, created, err := p.Allocate(ctx, partyOne, players)
			ids[i] = alloc.AllocationID
			createdFlags[i] = created
			errs[i] = err
		}(i)
	}
	wg.Wait()

	winner := ""
	createdCount := 0
	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d: %v", i, err)
			continue
		}
		if ids[i] == "" {
			t.Errorf("request %d returned no allocation id", i)
			continue
		}
		if winner == "" {
			winner = ids[i]
		} else if ids[i] != winner {
			t.Errorf("request %d allocation_id = %q, want %q", i, ids[i], winner)
		}
		if createdFlags[i] {
			createdCount++
		}
	}
	if winner == "" {
		t.Fatal("no request allocated")
	}
	if createdCount != 1 {
		t.Errorf("created was true %d times, want exactly 1", createdCount)
	}

	var live, reservedCount int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM allocation
		WHERE party_id = $1::text::uuid AND status IN ('reserved', 'active')`, partyOne).Scan(&live); err != nil {
		t.Fatalf("count live allocations: %v", err)
	}
	if live != 1 {
		t.Errorf("live allocations for the party = %d, want 1", live)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM game_server WHERE state = 'reserved'`).Scan(&reservedCount); err != nil {
		t.Fatalf("count reserved servers: %v", err)
	}
	if reservedCount != 1 {
		t.Errorf("reserved servers = %d, want 1", reservedCount)
	}
}

func TestGetAllocation(t *testing.T) {
	p, _ := newPool(t)
	mustRegister(t, p, serverAlpha)

	ctx := t.Context()
	alloc, _, err := p.Allocate(ctx, partyOne, []string{playerA, playerB})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	info, err := p.Get(ctx, alloc.AllocationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.AllocationID != alloc.AllocationID {
		t.Errorf("allocation_id = %q, want %q", info.AllocationID, alloc.AllocationID)
	}
	if info.PartyID != partyOne {
		t.Errorf("party_id = %q, want %q", info.PartyID, partyOne)
	}
	if info.ServerID != serverAlpha {
		t.Errorf("server_id = %q, want %q", info.ServerID, serverAlpha)
	}
	if info.Status != "reserved" {
		t.Errorf("status = %q, want reserved", info.Status)
	}
	if info.EndReason != nil {
		t.Errorf("end_reason = %q, want nil while live", *info.EndReason)
	}
	if info.EndedAt != nil {
		t.Errorf("ended_at = %s, want nil while live", *info.EndedAt)
	}
	if info.CreatedAt.IsZero() || info.ExpiresAt.IsZero() {
		t.Errorf("created_at/expires_at = %s/%s, want both set", info.CreatedAt, info.ExpiresAt)
	}
	got := slices.Clone(info.PlayerIDs)
	slices.Sort(got)
	if want := []string{playerA, playerB}; !slices.Equal(got, want) {
		t.Errorf("player_ids = %v, want %v", got, want)
	}
}

func TestGetAllocationErrors(t *testing.T) {
	p, _ := newPool(t)

	if _, err := p.Get(t.Context(), "99999999-9999-9999-9999-999999999999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get for an unknown allocation = %v, want ErrNotFound", err)
	}
	if _, err := p.Get(t.Context(), "not-a-uuid"); err == nil {
		t.Error("Get accepted a non-UUID allocation id")
	}
}

func TestLatestAllocation(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)

	ctx := t.Context()
	first, _, err := p.Allocate(ctx, partyOne, []string{playerA})
	if err != nil {
		t.Fatalf("first Allocate: %v", err)
	}
	if err := p.Ended(ctx, serverAlpha, first.AllocationID); err != nil {
		t.Fatalf("Ended: %v", err)
	}
	second, _, err := p.Allocate(ctx, partyOne, []string{playerB})
	if err != nil {
		t.Fatalf("second Allocate: %v", err)
	}

	// Backdate the ended row so "latest by created_at" cannot be decided by two
	// transactions landing on the same clock tick.
	if _, err := db.Exec(ctx, `
		UPDATE allocation SET created_at = now() - interval '1 minute'
		WHERE allocation_id = $1::text::uuid`, first.AllocationID); err != nil {
		t.Fatalf("backdate first allocation: %v", err)
	}

	info, err := p.Latest(ctx, partyOne)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if info.AllocationID != second.AllocationID {
		t.Errorf("latest allocation = %q, want the newer %q", info.AllocationID, second.AllocationID)
	}
	if info.Status != "reserved" {
		t.Errorf("status = %q, want reserved", info.Status)
	}

	if _, err := p.Latest(ctx, partyTwo); !errors.Is(err, ErrNotFound) {
		t.Errorf("Latest for a party with none = %v, want ErrNotFound", err)
	}
	if _, err := p.Latest(ctx, "not-a-uuid"); err == nil {
		t.Error("Latest accepted a non-UUID party id")
	}
}

// TestLatestAllocationReturnsEndedRow checks the repair-poll shape: the latest
// allocation is returned even after it ended, with its end_reason and ended_at
// populated.
func TestLatestAllocationReturnsEndedRow(t *testing.T) {
	p, _ := newPool(t)
	mustRegister(t, p, serverAlpha)

	ctx := t.Context()
	alloc, _, err := p.Allocate(ctx, partyOne, []string{playerA})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := p.Ended(ctx, serverAlpha, alloc.AllocationID); err != nil {
		t.Fatalf("Ended: %v", err)
	}

	info, err := p.Latest(ctx, partyOne)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if info.Status != "ended" {
		t.Errorf("status = %q, want ended", info.Status)
	}
	if info.EndReason == nil || *info.EndReason != "ended" {
		t.Errorf("end_reason = %v, want ended", info.EndReason)
	}
	if info.EndedAt == nil {
		t.Error("ended_at is nil for an ended allocation")
	}
}

func TestTicketTarget(t *testing.T) {
	p, _ := newPool(t)
	mustRegister(t, p, serverAlpha)

	ctx := t.Context()
	alloc, _, err := p.Allocate(ctx, partyOne, []string{playerA, playerB})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	t.Run("member of a live allocation", func(t *testing.T) {
		serverID, err := p.TicketTarget(ctx, alloc.AllocationID, strings.ToUpper(playerA))
		if err != nil {
			t.Fatalf("TicketTarget: %v", err)
		}
		if serverID != serverAlpha {
			t.Errorf("server_id = %q, want %q", serverID, serverAlpha)
		}
	})

	t.Run("unknown allocation", func(t *testing.T) {
		if _, err := p.TicketTarget(ctx, "99999999-9999-9999-9999-999999999999", playerA); !errors.Is(err, ErrNotFound) {
			t.Errorf("TicketTarget for an unknown allocation = %v, want ErrNotFound", err)
		}
	})

	t.Run("player not in the allocation", func(t *testing.T) {
		if _, err := p.TicketTarget(ctx, alloc.AllocationID, playerC); !errors.Is(err, ErrNotFound) {
			t.Errorf("TicketTarget for a non-member = %v, want ErrNotFound", err)
		}
	})

	t.Run("bad player id", func(t *testing.T) {
		if _, err := p.TicketTarget(ctx, alloc.AllocationID, "not-a-uuid"); err == nil {
			t.Error("TicketTarget accepted a non-UUID player id")
		}
	})

	t.Run("ended allocation", func(t *testing.T) {
		if err := p.Ended(ctx, serverAlpha, alloc.AllocationID); err != nil {
			t.Fatalf("Ended: %v", err)
		}
		if _, err := p.TicketTarget(ctx, alloc.AllocationID, playerA); !errors.Is(err, ErrAllocationEnded) {
			t.Errorf("TicketTarget for an ended allocation = %v, want ErrAllocationEnded", err)
		}
	})
}
