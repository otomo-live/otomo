// Package pool is the Allocator's game-server registry: the set of servers a party
// can be placed on and the lifecycle of the allocation each one holds.
//
// A game server owns its row in game_server. It registers to create or reset that row,
// then heartbeats to prove it is alive; the Allocator writes allocation rows when a
// party takes a server, and this package reads and ends them when a server restarts or
// dies. Every timestamp is taken from Postgres's now() rather than the Go clock,
// because several Allocator instances may share one database and must agree on what
// "stale" means.
//
// This package deliberately knows nothing about HTTP: it returns typed values and
// exported sentinel errors, and the api package turns those into status codes. That is
// what lets the registry logic be tested without a listener and the HTTP mapping be
// tested without a database.
package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/otomo-live/otomo/services/allocator/internal/metrics"
)

// The api layer maps these to status codes with errors.Is, so they are plain sentinel
// values rather than types carrying detail the client must not see.
var (
	// ErrNotRegistered means the server has no row at all, or its row is dead and
	// must be registered again before it can heartbeat. Registration is the only
	// thing that revives a dead row.
	ErrNotRegistered = errors.New("game server is not registered")

	// ErrAllocationMismatch means the allocation a server reported ended is not the
	// one it is currently holding and did not previously hold, so the report cannot
	// be trusted.
	ErrAllocationMismatch = errors.New("allocation does not belong to this server")
)

// serverIDPattern mirrors the CHECK constraint on game_server.server_id. Validating in
// Go as well lets the API answer 400 with a named field before touching Postgres, and
// means a caller gets one clear rejection instead of a constraint-violation log.
var serverIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// uuidPattern is a syntactic UUID check for request bodies. Postgres normalizes UUIDs
// itself, so this only has to decide "is this a UUID at all"; the store compares it as
// a uuid, not as text, and so is case-insensitive.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const (
	// minCapacity and maxCapacity mirror the game_server.capacity CHECK.
	minCapacity = 1
	maxCapacity = 64

	// maxPlayersConnected is deliberately looser than maxCapacity. The wire contract
	// accepts 0..1000 regardless of the server's advertised capacity, because a
	// server that has just taken a full party can report the count before the
	// Allocator's view of capacity catches up; the database only requires the count
	// to be non-negative.
	maxPlayersConnected = 1000
)

// Registration is a game server's register request. Its json tags are the wire field
// names, so the api package can decode straight into it and both packages share one
// definition of the payload.
type Registration struct {
	ServerID     string `json:"server_id"`
	InternalAddr string `json:"internal_addr"`
	Capacity     int    `json:"capacity"`
}

// Assignment is the live allocation a heartbeating server currently holds. It is nil
// when the server has none, which the API renders as JSON null rather than omitting
// the key, so a caller always has an allocation field to branch on.
type Assignment struct {
	AllocationID string    `json:"allocation_id"`
	PlayerIDs    []string  `json:"player_ids"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ServerInfo is one live server as the Gameplay Proxy needs it: which server it is,
// the address to forward a player's UDP traffic to, and its state so the proxy can
// tell a server that can accept a new player from one already held. Its json tags are
// the wire field names, so the api package can render it straight out.
type ServerInfo struct {
	ServerID     string `json:"server_id" db:"server_id"`
	InternalAddr string `json:"internal_addr" db:"internal_addr"`
	State        string `json:"state" db:"state"`
}

// Pool is the registry, backed by a shared pgx pool. Build it once with New; it holds
// no state of its own and is safe for concurrent use.
type Pool struct {
	db *pgxpool.Pool
	// timings are the durations Allocate compares against the database clock. They
	// live on the registry rather than being passed per call because a reservation's
	// eligibility and lifetime are properties of this deployment, not of a request.
	timings Timings
}

// New returns a registry backed by db. The pool is not owned: the caller that built it
// is responsible for closing it. t supplies the heartbeat timeout and reservation TTL
// the allocation path uses; the reaper keeps taking its timeout as an argument because
// it is driven by the run loop rather than by a request.
func New(db *pgxpool.Pool, t Timings) *Pool {
	return &Pool{db: db, timings: t}
}

// ValidateRegistration checks a register payload against the wire contract, returning
// an error that names the offending field. It is exported so the API can reject a bad
// request before opening a transaction and so tests can cover every case without a
// database.
func ValidateRegistration(reg Registration) error {
	if !serverIDPattern.MatchString(reg.ServerID) {
		return fmt.Errorf("server_id %q must be 1-63 characters matching %s", reg.ServerID, serverIDPattern)
	}
	if err := validateInternalAddr(reg.InternalAddr); err != nil {
		return err
	}
	if reg.Capacity < minCapacity || reg.Capacity > maxCapacity {
		return fmt.Errorf("capacity %d must be between %d and %d", reg.Capacity, minCapacity, maxCapacity)
	}
	return nil
}

// ValidatePlayersConnected checks a heartbeat's player count. The upper bound is the
// wire's 1000, not the server's capacity, so a heartbeat is never rejected for
// reporting more players than the server advertised.
func ValidatePlayersConnected(n int) error {
	if n < 0 || n > maxPlayersConnected {
		return fmt.Errorf("players_connected %d must be between 0 and %d", n, maxPlayersConnected)
	}
	return nil
}

// ValidateAllocationID checks that a reported allocation id is a UUID. The store
// compares it as a uuid, so a non-UUID would otherwise surface as a database error
// rather than a clean 400.
func ValidateAllocationID(id string) error {
	if !uuidPattern.MatchString(id) {
		return fmt.Errorf("allocation_id %q is not a UUID", id)
	}
	return nil
}

// validateInternalAddr enforces host:port with a usable port. A host is required —
// ":8080" is dialable in Go but names no game server — and the port must be in range,
// because handing a player an unroutable address is worse than rejecting the
// registration up front.
func validateInternalAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("internal_addr %q is not host:port: %v", addr, err)
	}
	if host == "" {
		return fmt.Errorf("internal_addr %q has an empty host", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("internal_addr %q has a non-numeric port", addr)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("internal_addr %q port %d must be between 1 and 65535", addr, n)
	}
	return nil
}

// Register creates or resets a game server's row in one transaction.
//
// Re-registering means the process restarted: its address and capacity may have
// changed, and any allocation it still held is dead because the process that held it
// is gone. So a live allocation is ended with server_restarted and the server is
// written back as free, at now(), which is also the clock the heartbeats and the
// reaper compare against.
func (p *Pool) Register(ctx context.Context, reg Registration) error {
	if err := ValidateRegistration(reg); err != nil {
		return err
	}

	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin register transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the existing row, if any, so a concurrent register or heartbeat cannot
	// interleave between ending the old allocation and writing the new row.
	var locked string
	err = tx.QueryRow(ctx, `SELECT server_id FROM game_server WHERE server_id = $1 FOR UPDATE`, reg.ServerID).Scan(&locked)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock game server %q: %w", reg.ServerID, err)
	}

	// A restart is the one event that invalidates an allocation without the game
	// server reporting it: the old process is gone, so a reserved or active hold on
	// it is dead. callback_pending puts it in the outbox so Session is told.
	if _, err := tx.Exec(ctx, `
		UPDATE allocation a
		SET status = 'ended',
		    end_reason = 'server_restarted',
		    ended_at = now(),
		    callback_pending = true
		FROM game_server g
		WHERE g.server_id = $1
		  AND a.allocation_id = g.allocation_id
		  AND a.status IN ('reserved', 'active')`, reg.ServerID); err != nil {
		return fmt.Errorf("end allocation of restarted game server %q: %w", reg.ServerID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO game_server (
			server_id, internal_addr, capacity, state,
			allocation_id, players_connected, registered_at, last_heartbeat
		)
		VALUES ($1, $2, $3, 'free', NULL, 0, now(), now())
		ON CONFLICT (server_id) DO UPDATE SET
			internal_addr     = EXCLUDED.internal_addr,
			capacity          = EXCLUDED.capacity,
			state             = 'free',
			allocation_id     = NULL,
			players_connected = 0,
			registered_at     = now(),
			last_heartbeat    = now()`,
		reg.ServerID, reg.InternalAddr, int16(reg.Capacity)); err != nil {
		return fmt.Errorf("upsert game server %q: %w", reg.ServerID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit register for %q: %w", reg.ServerID, err)
	}
	return nil
}

// ListServers returns every server not in state dead, ordered by server_id.
//
// The Gameplay Proxy routes on the ticket's srv claim, so it needs the internal_addr
// the matching server registered with. Servers register and restart at any time, so the
// proxy refreshes this list on a short period and whenever a ticket names a server it
// does not know. Dead servers are left out because the proxy must not forward a
// player's traffic to a process the reaper has already removed. The pool is a fixed,
// small set, so the whole list is returned and there is no paging.
func (p *Pool) ListServers(ctx context.Context) ([]ServerInfo, error) {
	rows, err := p.db.Query(ctx, `
		SELECT server_id, internal_addr, state
		FROM game_server
		WHERE state <> 'dead'
		ORDER BY server_id`)
	if err != nil {
		return nil, fmt.Errorf("list game servers: %w", err)
	}
	servers, err := pgx.CollectRows(rows, pgx.RowToStructByName[ServerInfo])
	if err != nil {
		return nil, fmt.Errorf("list game servers: %w", err)
	}
	return servers, nil
}

// Heartbeat records that a server is alive and returns the allocation it currently
// holds, or nil when it holds none.
//
// A dead server is reported as not registered rather than revived: only a fresh
// registration may bring it back, so a heartbeat cannot race the reaper and resurrect
// a server whose allocations were already ended. The first time a reserved server
// reports players, both sides move to active/busy, which is the signal the Allocator
// uses to stop counting the hold as merely reserved.
func (p *Pool) Heartbeat(ctx context.Context, serverID string, playersConnected int) (*Assignment, error) {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin heartbeat transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current, state string
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(allocation_id::text, ''), state
		FROM game_server
		WHERE server_id = $1
		FOR UPDATE`, serverID).Scan(&current, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotRegistered
	}
	if err != nil {
		return nil, fmt.Errorf("lock game server %q: %w", serverID, err)
	}
	if state == "dead" {
		return nil, ErrNotRegistered
	}

	// The heartbeat is recorded even when the server holds nothing: last_heartbeat is
	// what keeps the reaper away, and players_connected is the count the next
	// allocation decision sees.
	if _, err := tx.Exec(ctx, `
		UPDATE game_server
		SET last_heartbeat = now(), players_connected = $2
		WHERE server_id = $1`, serverID, int16(playersConnected)); err != nil {
		return nil, fmt.Errorf("record heartbeat for %q: %w", serverID, err)
	}

	var assignment *Assignment
	if current != "" {
		var a Assignment
		var status string
		err = tx.QueryRow(ctx, `
			SELECT allocation_id::text, player_ids::text[], expires_at, status
			FROM allocation
			WHERE allocation_id = $1::text::uuid
			FOR UPDATE`, current).Scan(&a.AllocationID, &a.PlayerIDs, &a.ExpiresAt, &status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// The pointer is dangling. This should not happen, but failing the
			// heartbeat would take a healthy server out of the pool over a bookkeeping
			// bug, so it is treated as "no allocation" instead.
		case err != nil:
			return nil, fmt.Errorf("read allocation %q: %w", current, err)
		default:
			if status == "reserved" && playersConnected > 0 {
				if _, err := tx.Exec(ctx, `UPDATE allocation SET status = 'active' WHERE allocation_id = $1::text::uuid`, current); err != nil {
					return nil, fmt.Errorf("activate allocation %q: %w", current, err)
				}
				if _, err := tx.Exec(ctx, `UPDATE game_server SET state = 'busy' WHERE server_id = $1`, serverID); err != nil {
					return nil, fmt.Errorf("mark game server %q busy: %w", serverID, err)
				}
				status = "active"
			}
			if status == "reserved" || status == "active" {
				assignment = &a
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit heartbeat for %q: %w", serverID, err)
	}
	return assignment, nil
}

// Ended frees a server when its game server reports the allocation is over.
//
// It is idempotent for the allocation the server actually held: a retry after a lost
// response finds the allocation already ended and is a no-op success. An allocation
// that is neither the server's current hold nor one of its ended holds is a mismatch,
// which is the signal that the report was forged or stale enough not to trust.
func (p *Pool) Ended(ctx context.Context, serverID, allocationID string) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin ended transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Ask Postgres whether it is the current allocation rather than comparing the
	// text ourselves: the id column is a uuid, so its canonical spelling is
	// lowercase and a client may send any case.
	var isCurrent bool
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(allocation_id = $2::text::uuid, false)
		FROM game_server
		WHERE server_id = $1
		FOR UPDATE`, serverID, allocationID).Scan(&isCurrent)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotRegistered
	}
	if err != nil {
		return fmt.Errorf("lock game server %q: %w", serverID, err)
	}

	if isCurrent {
		if _, err := tx.Exec(ctx, `
			UPDATE allocation
			SET status = 'ended',
			    end_reason = 'ended',
			    ended_at = now(),
			    callback_pending = true
			WHERE allocation_id = $1::text::uuid
			  AND status IN ('reserved', 'active')`, allocationID); err != nil {
			return fmt.Errorf("end allocation %q: %w", allocationID, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE game_server
			SET state = 'free', allocation_id = NULL, players_connected = 0
			WHERE server_id = $1`, serverID); err != nil {
			return fmt.Errorf("free game server %q: %w", serverID, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit ended for %q: %w", serverID, err)
		}
		return nil
	}

	// Not the server's current allocation. It may still be one the server held
	// earlier and already ended, which is the retry path; anything else is a
	// mismatch.
	var status string
	err = tx.QueryRow(ctx, `
		SELECT status
		FROM allocation
		WHERE allocation_id = $1::text::uuid AND server_id = $2`, allocationID, serverID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAllocationMismatch
	}
	if err != nil {
		return fmt.Errorf("look up allocation %q: %w", allocationID, err)
	}
	if status != "ended" && status != "expired" {
		return ErrAllocationMismatch
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ended retry for %q: %w", serverID, err)
	}
	return nil
}

// ReapDead marks every server whose last heartbeat predates now() - heartbeatTimeout
// as dead, ends each live allocation it held, and returns how many servers were
// reaped.
//
// The whole sweep is one statement, so it sees one snapshot and cannot half-apply: a
// server cannot be marked dead before the allocation it held is ended. The comparison
// uses Postgres's now(), not the Go clock, so two Allocator instances with skewed
// clocks reap the same set. heartbeatTimeout is passed as seconds to make_interval
// rather than as a duration, avoiding any dependency on how the driver encodes
// intervals.
func (p *Pool) ReapDead(ctx context.Context, heartbeatTimeout time.Duration) (int, error) {
	const reap = `
		WITH stale AS (
			SELECT server_id, allocation_id
			FROM game_server
			WHERE state <> 'dead'
			  AND last_heartbeat < now() - make_interval(secs => $1)
			FOR UPDATE
		),
		-- ended is deliberately not read by the final SELECT: Postgres runs a
		-- data-modifying CTE to completion whether or not anything references it, and
		-- leaving it unreferenced keeps the allocation update independent of the
		-- server update's row set.
		ended AS (
			UPDATE allocation a
			SET status = 'ended',
			    end_reason = 'server_dead',
			    ended_at = now(),
			    callback_pending = true
			FROM stale s
			WHERE a.allocation_id = s.allocation_id
			  AND a.status IN ('reserved', 'active')
			RETURNING a.allocation_id
		),
		reaped AS (
			UPDATE game_server g
			SET state = 'dead', allocation_id = NULL, players_connected = 0
			FROM stale s
			WHERE g.server_id = s.server_id
			RETURNING g.server_id
		)
		SELECT count(*) FROM reaped`

	var n int
	if err := p.db.QueryRow(ctx, reap, heartbeatTimeout.Seconds()).Scan(&n); err != nil {
		return 0, fmt.Errorf("reap dead game servers: %w", err)
	}
	return n, nil
}

// ExpireReservations expires every reservation whose TTL has passed and frees the
// server it was holding, returning how many allocations expired.
//
// A reservation exists only long enough for the party to connect, so once expires_at
// has passed the hold is dead whether or not the game server noticed. The whole sweep
// is one statement, like ReapDead: the expired allocations and the servers they held
// move together in one snapshot, so a server cannot be freed while its allocation is
// still counted as live, or the reverse.
//
// The due allocations are locked FOR UPDATE. That is the race with Heartbeat, which
// activates a reserved allocation the first time its server reports players: if the
// heartbeat commits first, READ COMMITTED re-checks this statement's WHERE against the
// new row and drops it from the set, so the activation wins. If expiry commits first,
// the heartbeat sees an expired allocation and reports no assignment.
//
// The final SELECT counts the expired allocations, not the freed servers: a reservation
// whose server has since re-registered and taken a different hold still expires, but its
// server is left alone because it no longer points at this allocation.
func (p *Pool) ExpireReservations(ctx context.Context) (int, error) {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin expiry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock order is game_server, then allocation: the same as Heartbeat, Register,
	// Ended and ReapDead, so expiry can never deadlock with them. Servers are locked
	// in server_id order so two Allocator instances expiring at once cannot deadlock
	// with each other either. A heartbeat that activates one of these reservations
	// first makes the re-checked row drop out (state is no longer 'reserved').
	if _, err := tx.Exec(ctx, `
		SELECT g.server_id
		FROM game_server g
		JOIN allocation a ON a.allocation_id = g.allocation_id
		WHERE g.state = 'reserved'
		  AND a.status = 'reserved'
		  AND a.expires_at < now()
		ORDER BY g.server_id
		FOR UPDATE OF g`); err != nil {
		return 0, fmt.Errorf("lock servers with expired reservations: %w", err)
	}

	// Every overdue reservation, including one no server points at any more.
	rows, err := tx.Query(ctx, `
		UPDATE allocation
		SET status = 'expired',
		    end_reason = 'expired',
		    ended_at = now(),
		    callback_pending = true
		WHERE status = 'reserved'
		  AND expires_at < now()
		RETURNING allocation_id::text`)
	if err != nil {
		return 0, fmt.Errorf("expire reservations: %w", err)
	}
	expired, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("expire reservations: %w", err)
	}
	if len(expired) == 0 {
		return 0, nil
	}

	// Frees only a server still pointing at the reservation it was held for. A server
	// that re-registered in between points elsewhere (or at nothing) and is no longer
	// 'reserved', so it is left alone.
	if _, err := tx.Exec(ctx, `
		UPDATE game_server
		SET state = 'free', allocation_id = NULL, players_connected = 0
		WHERE allocation_id = ANY($1::text[]::uuid[])
		  AND state = 'reserved'`, expired); err != nil {
		return 0, fmt.Errorf("free servers of expired reservations: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit expiry: %w", err)
	}
	return len(expired), nil
}

// RunReaper calls ReapDead every interval until ctx is done, logging what it found.
//
// A reaping error must not stop the loop: the most likely cause is a brief database
// outage, and the next tick is the retry. The loop also does not report the final
// reap's context cancellation as an error, because that is the normal shutdown path.
// Each tick reaps dead servers first and then expires stale reservations, so a server
// that died mid-reservation is ended by the reap rather than double-processed. The
// successful reap and expiry counts are recorded in m, which may be nil.
func (p *Pool) RunReaper(ctx context.Context, interval, heartbeatTimeout time.Duration, m *metrics.Metrics, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := p.ReapDead(ctx, heartbeatTimeout)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Error("reaping dead game servers failed", slog.Any("error", err))
				continue
			}
			m.RecordReaped(metrics.KindDead, n)
			if n > 0 {
				log.Info("reaped dead game servers", slog.Int("count", n))
			}

			expired, err := p.ExpireReservations(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Error("expiring reservations failed", slog.Any("error", err))
				continue
			}
			m.RecordReaped(metrics.KindExpired, expired)
			if expired > 0 {
				log.Info("expired reservations", slog.Int("count", expired))
			}
		}
	}
}
