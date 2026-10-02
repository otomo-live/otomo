package pool

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/otomo-live/otomo/services/allocator/internal/testdb"
)

// The test servers and players are fixed so a failure is easy to read and every UUID
// is already in the canonical lowercase Postgres returns.
const (
	serverAlpha = "gs-alpha"
	serverBeta  = "gs-beta"

	playerA = "11111111-1111-1111-1111-111111111111"
	playerB = "22222222-2222-2222-2222-222222222222"
)

func newPool(t *testing.T) (*Pool, *pgxpool.Pool) {
	t.Helper()
	db := testdb.Open(t)
	// The timings only matter to Allocate; the registry tests use them unchanged.
	return New(db, Timings{HeartbeatTimeout: 30 * time.Second, ReservationTTL: time.Minute}), db
}

// mustRegister puts a valid server in the pool, which most tests start from.
func mustRegister(t *testing.T, p *Pool, serverID string) {
	t.Helper()
	reg := Registration{ServerID: serverID, InternalAddr: "10.0.0.1:27015", Capacity: 8}
	if err := p.Register(t.Context(), reg); err != nil {
		t.Fatalf("Register %q: %v", serverID, err)
	}
}

type serverRow struct {
	state            string
	allocationID     string
	playersConnected int16
	internalAddr     string
	capacity         int16
}

func getServer(t *testing.T, db *pgxpool.Pool, serverID string) serverRow {
	t.Helper()
	var s serverRow
	err := db.QueryRow(t.Context(), `
		SELECT state, COALESCE(allocation_id::text, ''), players_connected, internal_addr, capacity
		FROM game_server
		WHERE server_id = $1`, serverID).
		Scan(&s.state, &s.allocationID, &s.playersConnected, &s.internalAddr, &s.capacity)
	if err != nil {
		t.Fatalf("read game server %q: %v", serverID, err)
	}
	return s
}

type allocationRow struct {
	status          string
	endReason       string
	ended           bool
	callbackPending bool
	serverID        string
}

func getAllocation(t *testing.T, db *pgxpool.Pool, allocationID string) allocationRow {
	t.Helper()
	var a allocationRow
	err := db.QueryRow(t.Context(), `
		SELECT status, COALESCE(end_reason, ''), ended_at IS NOT NULL, callback_pending, server_id
		FROM allocation
		WHERE allocation_id = $1`, allocationID).
		Scan(&a.status, &a.endReason, &a.ended, &a.callbackPending, &a.serverID)
	if err != nil {
		t.Fatalf("read allocation %q: %v", allocationID, err)
	}
	return a
}

// insertAllocation writes an allocation row directly. Allocation creation is a later
// ticket, so the registry tests build the state they need with SQL.
func insertAllocation(t *testing.T, db *pgxpool.Pool, serverID, status string, playerIDs []string) string {
	t.Helper()
	var id string
	err := db.QueryRow(t.Context(), `
		INSERT INTO allocation (allocation_id, party_id, server_id, player_ids, status, expires_at)
		VALUES (gen_random_uuid(), gen_random_uuid(), $1, $2::text[]::uuid[], $3, now() + interval '1 minute')
		RETURNING allocation_id::text`, serverID, playerIDs, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert allocation: %v", err)
	}
	return id
}

// assignServer points a server at an allocation without going through the reservation
// path, which does not exist yet.
func assignServer(t *testing.T, db *pgxpool.Pool, serverID, allocationID, state string) {
	t.Helper()
	if _, err := db.Exec(t.Context(), `
		UPDATE game_server
		SET allocation_id = $2::text::uuid, state = $3
		WHERE server_id = $1`, serverID, allocationID, state); err != nil {
		t.Fatalf("assign allocation %q to %q: %v", allocationID, serverID, err)
	}
}

// TestValidateRegistration pins every field rule without a database, so a rejection is
// attributed to the field the caller has to fix.
func TestValidateRegistration(t *testing.T) {
	if err := ValidateRegistration(Registration{ServerID: "gs-1", InternalAddr: "[::1]:27015", Capacity: 64}); err != nil {
		t.Fatalf("valid registration rejected: %v", err)
	}

	cases := []struct {
		name  string
		reg   Registration
		field string
	}{
		{"empty server id", Registration{ServerID: "", InternalAddr: "10.0.0.1:27015", Capacity: 8}, "server_id"},
		{"uppercase server id", Registration{ServerID: "GS-1", InternalAddr: "10.0.0.1:27015", Capacity: 8}, "server_id"},
		{"leading punctuation", Registration{ServerID: "-gs", InternalAddr: "10.0.0.1:27015", Capacity: 8}, "server_id"},
		{"over 63 characters", Registration{ServerID: strings.Repeat("a", 64), InternalAddr: "10.0.0.1:27015", Capacity: 8}, "server_id"},
		{"no port", Registration{ServerID: "gs-1", InternalAddr: "10.0.0.1", Capacity: 8}, "internal_addr"},
		{"empty host", Registration{ServerID: "gs-1", InternalAddr: ":27015", Capacity: 8}, "internal_addr"},
		{"non-numeric port", Registration{ServerID: "gs-1", InternalAddr: "10.0.0.1:http", Capacity: 8}, "internal_addr"},
		{"port zero", Registration{ServerID: "gs-1", InternalAddr: "10.0.0.1:0", Capacity: 8}, "internal_addr"},
		{"port too large", Registration{ServerID: "gs-1", InternalAddr: "10.0.0.1:70000", Capacity: 8}, "internal_addr"},
		{"capacity zero", Registration{ServerID: "gs-1", InternalAddr: "10.0.0.1:27015", Capacity: 0}, "capacity"},
		{"capacity too large", Registration{ServerID: "gs-1", InternalAddr: "10.0.0.1:27015", Capacity: 65}, "capacity"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRegistration(tc.reg)
			if err == nil {
				t.Fatalf("ValidateRegistration accepted %+v", tc.reg)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error does not name %s: %v", tc.field, err)
			}
		})
	}
}

// TestValidatePlayersConnectedAndAllocationID covers the two other wire checks the API
// shares with the store.
func TestValidatePlayersConnectedAndAllocationID(t *testing.T) {
	for _, n := range []int{0, 1, 1000} {
		if err := ValidatePlayersConnected(n); err != nil {
			t.Errorf("ValidatePlayersConnected(%d) = %v, want nil", n, err)
		}
	}
	for _, n := range []int{-1, 1001} {
		if err := ValidatePlayersConnected(n); err == nil {
			t.Errorf("ValidatePlayersConnected(%d) accepted an out-of-range count", n)
		}
	}

	for _, id := range []string{playerA, strings.ToUpper(playerA)} {
		if err := ValidateAllocationID(id); err != nil {
			t.Errorf("ValidateAllocationID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range []string{"", "not-a-uuid", playerA + "0"} {
		if err := ValidateAllocationID(id); err == nil {
			t.Errorf("ValidateAllocationID(%q) accepted a non-UUID", id)
		}
	}
}

func TestRegisterCreatesFreeServer(t *testing.T) {
	p, db := newPool(t)

	reg := Registration{ServerID: serverAlpha, InternalAddr: "10.0.0.1:27015", Capacity: 8}
	if err := p.Register(t.Context(), reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	s := getServer(t, db, serverAlpha)
	if s.state != "free" {
		t.Errorf("state = %q, want free", s.state)
	}
	if s.allocationID != "" {
		t.Errorf("allocation_id = %q, want none", s.allocationID)
	}
	if s.playersConnected != 0 {
		t.Errorf("players_connected = %d, want 0", s.playersConnected)
	}
	if s.internalAddr != reg.InternalAddr {
		t.Errorf("internal_addr = %q, want %q", s.internalAddr, reg.InternalAddr)
	}
	if s.capacity != int16(reg.Capacity) {
		t.Errorf("capacity = %d, want %d", s.capacity, reg.Capacity)
	}
}

// TestListServersSkipsDeadAndOrdersByID checks the read the Gameplay Proxy builds its
// routing table from: every live server's address and state, a dead server left out,
// and server_id order.
func TestListServersSkipsDeadAndOrdersByID(t *testing.T) {
	p, db := newPool(t)

	regs := []Registration{
		{ServerID: serverAlpha, InternalAddr: "10.0.0.1:27015", Capacity: 8},
		{ServerID: serverBeta, InternalAddr: "10.0.0.2:27016", Capacity: 8},
		{ServerID: serverGamma, InternalAddr: "10.0.0.3:27017", Capacity: 8},
	}
	for _, reg := range regs {
		if err := p.Register(t.Context(), reg); err != nil {
			t.Fatalf("Register %q: %v", reg.ServerID, err)
		}
	}
	// Mark beta dead and put gamma in a held state, so exclusion and state passthrough
	// are covered together.
	if _, err := db.Exec(t.Context(), `UPDATE game_server SET state = 'dead' WHERE server_id = $1`, serverBeta); err != nil {
		t.Fatalf("mark %q dead: %v", serverBeta, err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE game_server SET state = 'reserved' WHERE server_id = $1`, serverGamma); err != nil {
		t.Fatalf("reserve %q: %v", serverGamma, err)
	}

	got, err := p.ListServers(t.Context())
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	want := []ServerInfo{
		{ServerID: serverAlpha, InternalAddr: "10.0.0.1:27015", State: "free"},
		{ServerID: serverGamma, InternalAddr: "10.0.0.3:27017", State: "reserved"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ListServers = %+v, want %+v", got, want)
	}
}

// TestReRegisterEndsLiveAllocation checks the restart path: a live hold on the old
// process must end in the outbox, and the row must come back free with the new address
// and capacity.
func TestReRegisterEndsLiveAllocation(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	alloc := insertAllocation(t, db, serverAlpha, "active", []string{playerA})
	assignServer(t, db, serverAlpha, alloc, "busy")

	reg := Registration{ServerID: serverAlpha, InternalAddr: "10.0.0.2:27016", Capacity: 4}
	if err := p.Register(t.Context(), reg); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	a := getAllocation(t, db, alloc)
	if a.status != "ended" {
		t.Errorf("allocation status = %q, want ended", a.status)
	}
	if a.endReason != "server_restarted" {
		t.Errorf("end_reason = %q, want server_restarted", a.endReason)
	}
	if !a.ended {
		t.Error("ended_at is null")
	}
	if !a.callbackPending {
		t.Error("callback_pending is false; Session would never be told")
	}

	s := getServer(t, db, serverAlpha)
	if s.state != "free" || s.allocationID != "" || s.playersConnected != 0 {
		t.Errorf("server = %+v, want free with no allocation", s)
	}
	if s.internalAddr != reg.InternalAddr || s.capacity != int16(reg.Capacity) {
		t.Errorf("server = %+v, want %s capacity %d", s, reg.InternalAddr, reg.Capacity)
	}
}

func TestHeartbeatUnknownServerIsNotRegistered(t *testing.T) {
	p, _ := newPool(t)

	if _, err := p.Heartbeat(t.Context(), "nobody", 0); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Heartbeat on an unknown server = %v, want ErrNotRegistered", err)
	}
}

// TestHeartbeatOnDeadServerIsNotRegistered keeps the reaper final: only registration
// may revive a server, so a late heartbeat must not put it back in the pool.
func TestHeartbeatOnDeadServerIsNotRegistered(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	if _, err := db.Exec(t.Context(), `UPDATE game_server SET state = 'dead' WHERE server_id = $1`, serverAlpha); err != nil {
		t.Fatalf("mark dead: %v", err)
	}

	if _, err := p.Heartbeat(t.Context(), serverAlpha, 0); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Heartbeat on a dead server = %v, want ErrNotRegistered", err)
	}
}

// TestHeartbeatActivatesReservedAllocation checks the reserved -> active / busy
// transition: the first heartbeat that reports players is the signal the hold is real.
func TestHeartbeatActivatesReservedAllocation(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	alloc := insertAllocation(t, db, serverAlpha, "reserved", []string{playerA})
	assignServer(t, db, serverAlpha, alloc, "reserved")

	got, err := p.Heartbeat(t.Context(), serverAlpha, 2)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got == nil || got.AllocationID != alloc {
		t.Fatalf("Heartbeat = %+v, want allocation %q", got, alloc)
	}

	if a := getAllocation(t, db, alloc); a.status != "active" {
		t.Errorf("allocation status = %q, want active", a.status)
	}
	if s := getServer(t, db, serverAlpha); s.state != "busy" {
		t.Errorf("server state = %q, want busy", s.state)
	}
}

// TestHeartbeatReturnsAssignment pins the fields the API hands back, and that a
// heartbeat with no hold is a clean nil rather than an error.
func TestHeartbeatReturnsAssignment(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)

	if got, err := p.Heartbeat(t.Context(), serverAlpha, 0); err != nil || got != nil {
		t.Fatalf("Heartbeat without an allocation = (%+v, %v), want (nil, nil)", got, err)
	}

	alloc := insertAllocation(t, db, serverAlpha, "active", []string{playerA, playerB})
	assignServer(t, db, serverAlpha, alloc, "busy")

	got, err := p.Heartbeat(t.Context(), serverAlpha, 2)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got == nil {
		t.Fatal("Heartbeat returned no allocation for a busy server")
	}
	if got.AllocationID != alloc {
		t.Errorf("allocation_id = %q, want %q", got.AllocationID, alloc)
	}
	if !slices.Equal(got.PlayerIDs, []string{playerA, playerB}) {
		t.Errorf("player_ids = %v, want [%s %s]", got.PlayerIDs, playerA, playerB)
	}
	if got.ExpiresAt.IsZero() {
		t.Error("expires_at is the zero time")
	}
}

func TestEndedCurrentAllocationFreesServer(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	alloc := insertAllocation(t, db, serverAlpha, "active", []string{playerA})
	assignServer(t, db, serverAlpha, alloc, "busy")

	if err := p.Ended(t.Context(), serverAlpha, alloc); err != nil {
		t.Fatalf("Ended: %v", err)
	}

	a := getAllocation(t, db, alloc)
	if a.status != "ended" || a.endReason != "ended" || !a.ended || !a.callbackPending {
		t.Errorf("allocation = %+v, want ended/ended with callback pending", a)
	}
	if a.serverID != serverAlpha {
		t.Errorf("allocation.server_id = %q, want %q", a.serverID, serverAlpha)
	}
	s := getServer(t, db, serverAlpha)
	if s.state != "free" || s.allocationID != "" || s.playersConnected != 0 {
		t.Errorf("server = %+v, want free with no allocation", s)
	}
}

// TestEndedIsIdempotent covers a retry after a lost response: the second report is a
// success, not a mismatch.
func TestEndedIsIdempotent(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	alloc := insertAllocation(t, db, serverAlpha, "active", []string{playerA})
	assignServer(t, db, serverAlpha, alloc, "busy")

	if err := p.Ended(t.Context(), serverAlpha, alloc); err != nil {
		t.Fatalf("first Ended: %v", err)
	}
	if err := p.Ended(t.Context(), serverAlpha, alloc); err != nil {
		t.Fatalf("retried Ended = %v, want nil", err)
	}
}

// TestEndedMismatchRejectsForeignAndUnknownAllocations checks the guard: a report for
// something the server never held cannot free it, and cannot be mistaken for a retry.
func TestEndedMismatchRejectsForeignAndUnknownAllocations(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	mustRegister(t, p, serverBeta)

	alloc := insertAllocation(t, db, serverAlpha, "active", []string{playerA})
	assignServer(t, db, serverAlpha, alloc, "busy")

	if err := p.Ended(t.Context(), serverBeta, alloc); !errors.Is(err, ErrAllocationMismatch) {
		t.Errorf("Ended by a foreign server = %v, want ErrAllocationMismatch", err)
	}
	if err := p.Ended(t.Context(), serverAlpha, "99999999-9999-9999-9999-999999999999"); !errors.Is(err, ErrAllocationMismatch) {
		t.Errorf("Ended for an unknown allocation = %v, want ErrAllocationMismatch", err)
	}

	// A rejected report must not have ended the real allocation.
	if a := getAllocation(t, db, alloc); a.status != "active" {
		t.Errorf("allocation status = %q after rejected reports, want active", a.status)
	}
}

// TestReapDeadEndsAllocationAndSparesFreshServer checks the timeout boundary: a stale
// server dies with its allocation, and a server that just heartbeated is untouched.
func TestReapDeadEndsAllocationAndSparesFreshServer(t *testing.T) {
	p, db := newPool(t)
	mustRegister(t, p, serverAlpha)
	mustRegister(t, p, serverBeta)

	alloc := insertAllocation(t, db, serverBeta, "active", []string{playerA})
	assignServer(t, db, serverBeta, alloc, "busy")

	// A timeout of 30s against a heartbeat a minute old, so the test cannot be made
	// flaky by a slow machine between the register and the reap.
	if _, err := db.Exec(t.Context(), `
		UPDATE game_server SET last_heartbeat = now() - interval '1 minute'
		WHERE server_id = $1`, serverBeta); err != nil {
		t.Fatalf("make stale: %v", err)
	}

	n, err := p.ReapDead(t.Context(), 30*time.Second)
	if err != nil {
		t.Fatalf("ReapDead: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapDead reaped %d servers, want 1", n)
	}

	s := getServer(t, db, serverBeta)
	if s.state != "dead" || s.allocationID != "" || s.playersConnected != 0 {
		t.Errorf("stale server = %+v, want dead with no allocation", s)
	}
	a := getAllocation(t, db, alloc)
	if a.status != "ended" || a.endReason != "server_dead" || !a.callbackPending {
		t.Errorf("allocation = %+v, want ended/server_dead with callback pending", a)
	}

	if fresh := getServer(t, db, serverAlpha); fresh.state != "free" {
		t.Errorf("fresh server state = %q, want free", fresh.state)
	}
}
