// The allocation half of the registry: reserving a server for a party, reading an
// allocation back, and deciding whether a player may be handed a fresh ticket to it.
//
// Every clock read stays in Postgres. A reservation's expires_at is now() + TTL and a
// server is eligible only while last_heartbeat > now() - timeout, so two Allocator
// instances with skewed clocks still agree on which holds are live and which servers
// are usable. The concurrency guarantee is a pair of database primitives: the free
// server is chosen with FOR UPDATE SKIP LOCKED so no two requests can take the same
// one, and the unique index on live parties rejects a second hold for a party that
// won a race, which Allocate turns into the idempotent path.
package pool

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The api layer maps these to status codes with errors.Is, so they are plain sentinel
// values rather than types carrying detail the client must not see.
var (
	// ErrNoCapacity means no free server had enough room for the party and a fresh
	// enough heartbeat. It is distinct from a database error so Session can tell
	// "the pool is full, retry later" from "something is broken".
	ErrNoCapacity = errors.New("no free game server with enough capacity")

	// ErrAllocationConflict means the party already holds a live allocation for a
	// different set of players. Reusing a party id with a different roster is a
	// client bug and must not overwrite the hold the original players are using.
	ErrAllocationConflict = errors.New("party has a live allocation with a different player set")

	// ErrNotFound means no allocation matched the id or party asked about. It covers
	// both "no such id" and "that player was never in this allocation", because the
	// caller cannot act differently on the two.
	ErrNotFound = errors.New("allocation not found")

	// ErrAllocationEnded means the allocation is real but no longer live. It is a
	// conflict rather than a not-found because the caller's reference is valid, just
	// spent.
	ErrAllocationEnded = errors.New("allocation is not live")
)

// maxPartySize is the largest party the schema and the wire contract allow. It is a
// constant rather than a configuration value because the allocation table's CHECK and
// ValidateAllocationRequest must never disagree.
const maxPartySize = 8

// Timings are the durations the allocation path compares against the database clock.
// They are injected rather than read from config inside this package, so the registry
// stays independent of how the service is configured and the tests can pin them.
type Timings struct {
	// HeartbeatTimeout is how stale a server's last heartbeat may be before it is no
	// longer eligible for a new reservation.
	HeartbeatTimeout time.Duration

	// ReservationTTL is how long a fresh reservation is held before it expires.
	ReservationTTL time.Duration
}

// Allocation is a party's hold on a server as the create response reports it. It
// carries only what the registry owns; the Gameplay Proxy's public address and the
// per-player tickets are added by the api layer, which is where the issuer lives.
type Allocation struct {
	AllocationID string    `json:"allocation_id"`
	ServerID     string    `json:"server_id"`
	Status       string    `json:"status"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// AllocationInfo is the full row the GET routes return. EndReason and EndedAt are
// pointers so a live allocation renders an explicit JSON null rather than a zero value
// that reads like a real end reason or timestamp.
type AllocationInfo struct {
	AllocationID string     `json:"allocation_id"`
	PartyID      string     `json:"party_id"`
	ServerID     string     `json:"server_id"`
	Status       string     `json:"status"`
	EndReason    *string    `json:"end_reason"`
	PlayerIDs    []string   `json:"player_ids"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	EndedAt      *time.Time `json:"ended_at"`
}

// ValidatePartyID checks that a party id is a UUID. It is exported so the api layer can
// answer 400 naming party_id before the store sees it; Postgres would otherwise report
// the bad cast as an internal error.
func ValidatePartyID(partyID string) error {
	if !uuidPattern.MatchString(partyID) {
		return fmt.Errorf("party_id %q is not a UUID", partyID)
	}
	return nil
}

// ValidatePlayerID checks that a single player id is a UUID, for the ticket route's
// body. The store compares player_ids as a uuid[], so a non-UUID has to be rejected in
// Go rather than discovered by the cast.
func ValidatePlayerID(playerID string) error {
	if !uuidPattern.MatchString(playerID) {
		return fmt.Errorf("player_id %q is not a UUID", playerID)
	}
	return nil
}

// ValidateAllocationRequest checks a create-allocation request against the wire
// contract and returns the player ids normalised to lowercase and sorted.
//
// Returning a sorted slice is what lets Allocate compare the request against a stored
// allocation with slices.Equal: the contract cares about the set of players, not their
// order, and Postgres stores uuids in their canonical lowercase spelling, so lowering
// the request is enough to make the two representations comparable.
func ValidateAllocationRequest(partyID string, playerIDs []string) ([]string, error) {
	if err := ValidatePartyID(partyID); err != nil {
		return nil, err
	}
	if len(playerIDs) < 1 || len(playerIDs) > maxPartySize {
		return nil, fmt.Errorf("player_ids must have between 1 and %d players", maxPartySize)
	}

	normalised := make([]string, 0, len(playerIDs))
	seen := make(map[string]struct{}, len(playerIDs))
	for _, id := range playerIDs {
		if !uuidPattern.MatchString(id) {
			return nil, fmt.Errorf("player_ids contains %q, which is not a UUID", id)
		}
		id = strings.ToLower(id)
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("player_ids contains %q more than once", id)
		}
		seen[id] = struct{}{}
		normalised = append(normalised, id)
	}
	slices.Sort(normalised)
	return normalised, nil
}

// Allocate reserves a server for a party, or returns the party's existing live
// allocation when the request is an idempotent retry.
//
// created reports which happened, so the api layer can answer 201 or 200. The
// reservation is one transaction: the free-server pick is FOR UPDATE SKIP LOCKED, so
// concurrent requests never choose the same server, and the unique index on live
// parties is the other half of the guarantee, rejecting a second hold for one party.
//
// Requests for the same party are serialised by an advisory lock (see allocate). If
// one still slips between the "no live allocation" check and the insert, the loser's
// insert fails with 23505 on allocation_live_party. That is not an error:
// the whole operation is redone once, at which point the winner's committed row is
// visible and takes the idempotent path. A second loss means the winner ended its
// allocation in between and the retry genuinely lost capacity again, which is
// reported rather than retried forever.
func (p *Pool) Allocate(ctx context.Context, partyID string, playerIDs []string) (Allocation, bool, error) {
	players, err := ValidateAllocationRequest(partyID, playerIDs)
	if err != nil {
		return Allocation{}, false, err
	}

	for attempt := 0; attempt < 2; attempt++ {
		alloc, created, err := p.allocate(ctx, partyID, players)
		if err == nil {
			return alloc, created, nil
		}
		if !isLivePartyConflict(err) {
			return Allocation{}, false, err
		}
	}
	return Allocation{}, false, fmt.Errorf("allocate for party %q: lost the live-allocation race twice", partyID)
}

// allocate runs one reservation attempt in its own transaction. A 23505 on the live
// party index is returned unchanged so Allocate can recognise it and retry; every
// other error is wrapped with the context needed to debug it.
func (p *Pool) allocate(ctx context.Context, partyID string, players []string) (Allocation, bool, error) {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return Allocation{}, false, fmt.Errorf("begin allocate transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialise requests for the same party before looking at anything. Without this,
	// concurrent retries for one party all see "no live allocation"; the first locks
	// the only free server and the rest skip it (SKIP LOCKED) and report no_capacity
	// instead of waiting to find the first one's allocation. A transaction-scoped
	// advisory lock keyed on the party makes a duplicate wait for the first to commit
	// and then take the idempotent path, while different parties still reserve in
	// parallel. The 23505 retry in Allocate stays as the backstop.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, partyID); err != nil {
		return Allocation{}, false, fmt.Errorf("lock party %q: %w", partyID, err)
	}

	existing, existingPlayers, err := liveAllocation(ctx, tx, partyID)
	switch {
	case err == nil:
		if !samePlayerSet(existingPlayers, players) {
			return Allocation{}, false, ErrAllocationConflict
		}
		// The committed allocation already satisfies the request. Tickets are
		// stateless and cheap, so the api layer mints fresh ones from this result;
		// there is nothing to write, and the deferred rollback is the cleanest exit.
		return existing, false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Allocation{}, false, err
	}

	// SKIP LOCKED is the reservation guarantee: a server another transaction is
	// holding is skipped rather than waited for, so two requests cannot both take
	// the same free server, and a request that finds nothing is free to answer
	// no_capacity immediately.
	var serverID string
	err = tx.QueryRow(ctx, `
		SELECT server_id
		FROM game_server
		WHERE state = 'free'
		  AND capacity >= $1
		  AND last_heartbeat > now() - make_interval(secs => $2)
		ORDER BY server_id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
		int16(len(players)), p.timings.HeartbeatTimeout.Seconds()).Scan(&serverID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, false, ErrNoCapacity
	}
	if err != nil {
		return Allocation{}, false, fmt.Errorf("reserve a free game server for party %q: %w", partyID, err)
	}

	var alloc Allocation
	err = tx.QueryRow(ctx, `
		INSERT INTO allocation (
			allocation_id, party_id, server_id, player_ids, status, expires_at
		)
		VALUES (
			gen_random_uuid(), $1::text::uuid, $2, $3::text[]::uuid[], 'reserved',
			now() + make_interval(secs => $4)
		)
		RETURNING allocation_id::text, server_id, status, expires_at`,
		partyID, serverID, players, p.timings.ReservationTTL.Seconds()).
		Scan(&alloc.AllocationID, &alloc.ServerID, &alloc.Status, &alloc.ExpiresAt)
	if err != nil {
		// Unchanged: Allocate inspects this for the live-party unique violation to
		// tell a lost race from a real database failure.
		return Allocation{}, false, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE game_server
		SET state = 'reserved', allocation_id = $1::text::uuid
		WHERE server_id = $2`, alloc.AllocationID, serverID); err != nil {
		return Allocation{}, false, fmt.Errorf("mark game server %q reserved: %w", serverID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Allocation{}, false, fmt.Errorf("commit allocation for party %q: %w", partyID, err)
	}
	return alloc, true, nil
}

// liveAllocation reads and locks a party's live allocation, if it has one. A party
// with no hold yields pgx.ErrNoRows, which Allocate reads as "reserve a server".
func liveAllocation(ctx context.Context, tx pgx.Tx, partyID string) (Allocation, []string, error) {
	var alloc Allocation
	var players []string
	err := tx.QueryRow(ctx, `
		SELECT allocation_id::text, server_id, status, expires_at, player_ids::text[]
		FROM allocation
		WHERE party_id = $1::text::uuid
		  AND status IN ('reserved', 'active')
		FOR UPDATE`, partyID).
		Scan(&alloc.AllocationID, &alloc.ServerID, &alloc.Status, &alloc.ExpiresAt, &players)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, nil, err
	}
	if err != nil {
		return Allocation{}, nil, fmt.Errorf("read live allocation for party %q: %w", partyID, err)
	}
	return alloc, players, nil
}

// samePlayerSet compares a stored player list with the request's normalised one. Both
// sides are lowercase; the stored slice is not guaranteed sorted, so it is copied
// before sorting rather than reordered in place.
func samePlayerSet(stored, requested []string) bool {
	if len(stored) != len(requested) {
		return false
	}
	sorted := slices.Clone(stored)
	slices.Sort(sorted)
	return slices.Equal(sorted, requested)
}

// isLivePartyConflict reports whether err is the unique violation the live-party index
// raises when two requests race to create the same party's allocation. The constraint
// name is checked as well as the code, so a future unique index on this table cannot be
// mistaken for this one.
func isLivePartyConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "allocation_live_party"
}

// Get returns one allocation by id, live or ended. The api layer validates the id
// before calling so a malformed one is a 400; the store re-checks so a direct caller
// cannot hand Postgres a bad uuid.
func (p *Pool) Get(ctx context.Context, id string) (AllocationInfo, error) {
	if err := ValidateAllocationID(id); err != nil {
		return AllocationInfo{}, err
	}

	var info AllocationInfo
	err := p.db.QueryRow(ctx, `
		SELECT allocation_id::text, party_id::text, server_id, status, end_reason,
		       player_ids::text[], created_at, expires_at, ended_at
		FROM allocation
		WHERE allocation_id = $1::text::uuid`, id).
		Scan(&info.AllocationID, &info.PartyID, &info.ServerID, &info.Status,
			&info.EndReason, &info.PlayerIDs, &info.CreatedAt, &info.ExpiresAt, &info.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AllocationInfo{}, ErrNotFound
	}
	if err != nil {
		return AllocationInfo{}, fmt.Errorf("get allocation %q: %w", id, err)
	}
	return info, nil
}

// Latest returns a party's most recently created allocation, live or ended. It backs
// Session's repair poll for a callback it missed: the latest row is the one whose end
// it was never told about. allocation_id breaks a created_at tie so the answer is
// deterministic even when two rows share the database's transaction timestamp.
func (p *Pool) Latest(ctx context.Context, partyID string) (AllocationInfo, error) {
	if err := ValidatePartyID(partyID); err != nil {
		return AllocationInfo{}, err
	}

	var info AllocationInfo
	err := p.db.QueryRow(ctx, `
		SELECT allocation_id::text, party_id::text, server_id, status, end_reason,
		       player_ids::text[], created_at, expires_at, ended_at
		FROM allocation
		WHERE party_id = $1::text::uuid
		ORDER BY created_at DESC, allocation_id DESC
		LIMIT 1`, partyID).
		Scan(&info.AllocationID, &info.PartyID, &info.ServerID, &info.Status,
			&info.EndReason, &info.PlayerIDs, &info.CreatedAt, &info.ExpiresAt, &info.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AllocationInfo{}, ErrNotFound
	}
	if err != nil {
		return AllocationInfo{}, fmt.Errorf("get latest allocation for party %q: %w", partyID, err)
	}
	return info, nil
}

// TicketTarget validates that playerID belongs to a live allocation and returns the
// server that allocation is on, which is what an issued ticket names as its target.
//
// Membership is checked before liveness, so a player who was never in the allocation
// gets not_found rather than allocation_ended; only a real member of a spent
// allocation is told the allocation ended.
func (p *Pool) TicketTarget(ctx context.Context, allocationID, playerID string) (string, error) {
	if err := ValidateAllocationID(allocationID); err != nil {
		return "", err
	}
	if err := ValidatePlayerID(playerID); err != nil {
		return "", err
	}

	var serverID, status string
	var players []string
	err := p.db.QueryRow(ctx, `
		SELECT server_id, status, player_ids::text[]
		FROM allocation
		WHERE allocation_id = $1::text::uuid`, allocationID).
		Scan(&serverID, &status, &players)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read allocation %q for a ticket: %w", allocationID, err)
	}

	// Stored player_ids are a uuid[], so they come back in Postgres's canonical
	// lowercase; normalising the request is enough to compare them as text.
	if !slices.Contains(players, strings.ToLower(playerID)) {
		return "", ErrNotFound
	}
	if status != "reserved" && status != "active" {
		return "", ErrAllocationEnded
	}
	return serverID, nil
}
