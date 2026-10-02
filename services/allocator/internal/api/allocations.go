// Allocation routes: the endpoints Session calls to place a party on a game server,
// look an allocation up, and re-mint a player's join ticket. They are the Session-side
// counterpart to the game-server registry in servers.go, and every one of them is
// behind the session service key.
//
// The HTTP layer owns two things the registry deliberately does not: the Gameplay
// Proxy's public address (a deployment fact) and ticket issuance (a signing key). It
// therefore assembles the create response itself, minting tickets only after the
// reservation has committed.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/otomo-live/otomo/services/allocator/internal/metrics"
	"github.com/otomo-live/otomo/services/allocator/internal/pool"
	"github.com/otomo-live/otomo/services/allocator/internal/servicekey"
	"github.com/otomo-live/otomo/services/allocator/internal/token"
)

// AllocationStore is the slice of the registry the allocation routes need. It is an
// interface so the request/response mapping can be tested with a fake and the registry
// can be tested without a listener; pool.Pool satisfies it.
type AllocationStore interface {
	Allocate(ctx context.Context, partyID string, playerIDs []string) (pool.Allocation, bool, error)
	Get(ctx context.Context, id string) (pool.AllocationInfo, error)
	Latest(ctx context.Context, partyID string) (pool.AllocationInfo, error)
	TicketTarget(ctx context.Context, allocationID, playerID string) (string, error)
}

// TicketIssuer mints one player's join ticket for an allocation on a server.
// token.Issuer is the production implementation; the interface exists so the handlers
// can be exercised without signing keys.
type TicketIssuer interface {
	Issue(playerID, allocationID, serverID string) (string, time.Time, error)
}

// The production implementations. The assertions keep the interfaces and what main
// passes to them from drifting apart without waiting for the wiring to fail to build.
var (
	_ AllocationStore = (*pool.Pool)(nil)
	_ TicketIssuer    = (*token.Issuer)(nil)
)

// allocationResponse is the create-allocation body. Address and Port are the configured
// public Gameplay Proxy endpoint, never the reserved server's internal_addr, and
// Tickets maps each normalised player id to a freshly signed ticket.
type allocationResponse struct {
	AllocationID string            `json:"allocation_id"`
	ServerID     string            `json:"server_id"`
	Status       string            `json:"status"`
	Address      string            `json:"address"`
	Port         int               `json:"port"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Tickets      map[string]string `json:"tickets"`
}

// ticketResponse is the re-issued-ticket body for one player.
type ticketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RegisterAllocationRoutes mounts the four Session-facing allocation routes on mux,
// each behind the session key.
//
// publicAddress and publicPort are the Gameplay Proxy's dialable endpoint and are
// echoed in every create response; issuer mints the per-player tickets. log is used
// only for unexpected store or issuer failures, which are answered with a generic 500.
// A nil log falls back to slog.Default, matching the server's own defaulting. m records
// the create route's outcome and handling time and may be nil.
func RegisterAllocationRoutes(mux *http.ServeMux, keys *servicekey.Keys, store AllocationStore, issuer TicketIssuer, publicAddress string, publicPort int, m *metrics.Metrics, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}

	mux.Handle("POST /internal/allocations",
		keys.Require(servicekey.RoleSession, handleAllocate(store, issuer, publicAddress, publicPort, m, log)))
	mux.Handle("GET /internal/allocations/{allocation_id}",
		keys.Require(servicekey.RoleSession, handleGetAllocation(store, log)))
	mux.Handle("GET /internal/allocations",
		keys.Require(servicekey.RoleSession, handleLatestAllocation(store, log)))
	mux.Handle("POST /internal/allocations/{allocation_id}/tickets",
		keys.Require(servicekey.RoleSession, handleAllocationTicket(store, issuer, log)))
}

// allocateRequest is the create-allocation body. Its json tags are the wire field
// names, which is what lets decodeBody report a misspelling by name.
type allocateRequest struct {
	PartyID   string   `json:"party_id"`
	PlayerIDs []string `json:"player_ids"`
}

// handleAllocate reserves a server for a party and returns the public endpoint plus
// one ticket per player.
//
// The body is validated here as well as in the store, so a bad request is a 400 naming
// the field rather than a 500 from Postgres; the store re-checks for direct callers.
// A 201 means a fresh reservation, a 200 an idempotent retry, and both carry new
// tickets because a stateless ticket is cheaper to mint than to look up.
//
// The outcome and the handler's wall time are recorded on the deferred return, so a
// body that never parses is still counted as invalid rather than vanishing.
func handleAllocate(store AllocationStore, issuer TicketIssuer, publicAddress string, publicPort int, m *metrics.Metrics, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// result defaults to error so an unclassified path is still counted.
		result := metrics.ResultError
		defer func() { m.RecordAllocation(result, time.Since(start)) }()

		var req allocateRequest
		if !decodeBody(w, r, &req) {
			result = metrics.ResultInvalid
			return
		}
		players, err := pool.ValidateAllocationRequest(req.PartyID, req.PlayerIDs)
		if err != nil {
			result = metrics.ResultInvalid
			WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		alloc, created, err := store.Allocate(r.Context(), req.PartyID, players)
		if err != nil {
			result = allocationResult(err)
			writeAllocationError(w, r, err, log)
			return
		}

		tickets, err := issueTickets(issuer, players, alloc)
		if err != nil {
			// The reservation is already committed and cannot be undone by a failed
			// signing operation, so this is an error to report, not to roll back.
			log.ErrorContext(r.Context(), "issuing join tickets failed",
				slog.Any("error", err),
				slog.String("allocation_id", alloc.AllocationID),
				slog.String("request_id", RequestID(r.Context())),
			)
			WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}

		if created {
			result = metrics.ResultCreated
		} else {
			result = metrics.ResultExisting
		}
		status := http.StatusCreated
		if !created {
			status = http.StatusOK
		}
		WriteJSON(w, status, allocationResponse{
			AllocationID: alloc.AllocationID,
			ServerID:     alloc.ServerID,
			Status:       alloc.Status,
			Address:      publicAddress,
			Port:         publicPort,
			ExpiresAt:    alloc.ExpiresAt,
			Tickets:      tickets,
		})
	}
}

// allocationResult maps the store's create sentinels onto the result label. Anything
// else — including the errors writeAllocationError renders as a generic 500 — is
// "error".
func allocationResult(err error) string {
	switch {
	case errors.Is(err, pool.ErrNoCapacity):
		return metrics.ResultNoCapacity
	case errors.Is(err, pool.ErrAllocationConflict):
		return metrics.ResultConflict
	default:
		return metrics.ResultError
	}
}

// issueTickets mints one ticket per normalised player id. A partial failure abandons
// the whole map rather than returning the tickets already signed: a response missing
// one player's ticket would silently leave that player unable to join.
func issueTickets(issuer TicketIssuer, players []string, alloc pool.Allocation) (map[string]string, error) {
	tickets := make(map[string]string, len(players))
	for _, playerID := range players {
		ticket, _, err := issuer.Issue(playerID, alloc.AllocationID, alloc.ServerID)
		if err != nil {
			return nil, err
		}
		tickets[playerID] = ticket
	}
	return tickets, nil
}

// handleGetAllocation returns one allocation, live or ended. The id is validated here
// so a non-UUID is a 400 rather than a Postgres cast error logged as a 500.
func handleGetAllocation(store AllocationStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("allocation_id")
		if err := pool.ValidateAllocationID(id); err != nil {
			WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		info, err := store.Get(r.Context(), id)
		if err != nil {
			writeAllocationError(w, r, err, log)
			return
		}
		WriteJSON(w, http.StatusOK, info)
	}
}

// handleLatestAllocation returns a party's latest allocation, which is Session's
// repair path when it missed an end callback. party_id is required and validated here
// so a missing or malformed one is a clean 400.
func handleLatestAllocation(store AllocationStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		partyID := r.URL.Query().Get("party_id")
		if partyID == "" {
			WriteError(w, r, http.StatusBadRequest, "invalid_request", "party_id is required")
			return
		}
		if err := pool.ValidatePartyID(partyID); err != nil {
			WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		info, err := store.Latest(r.Context(), partyID)
		if err != nil {
			writeAllocationError(w, r, err, log)
			return
		}
		WriteJSON(w, http.StatusOK, info)
	}
}

// ticketRequest is the re-issue body. The allocation id comes from the path, so the
// body names only the player.
type ticketRequest struct {
	PlayerID string `json:"player_id"`
}

// handleAllocationTicket re-mints one player's ticket for a live allocation. The store
// decides membership and liveness, so an unknown allocation or a player who was never
// in it is a 404 and a spent allocation is a 409.
func handleAllocationTicket(store AllocationStore, issuer TicketIssuer, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ticketRequest
		if !decodeBody(w, r, &req) {
			return
		}
		allocationID := r.PathValue("allocation_id")
		if err := pool.ValidateAllocationID(allocationID); err != nil {
			WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err := pool.ValidatePlayerID(req.PlayerID); err != nil {
			WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		serverID, err := store.TicketTarget(r.Context(), allocationID, req.PlayerID)
		if err != nil {
			writeAllocationError(w, r, err, log)
			return
		}

		// Normalise the subject so the ticket matches the canonical player id the
		// allocation stored, whatever case the caller sent.
		ticket, expiresAt, err := issuer.Issue(strings.ToLower(req.PlayerID), allocationID, serverID)
		if err != nil {
			log.ErrorContext(r.Context(), "re-issuing a join ticket failed",
				slog.Any("error", err),
				slog.String("allocation_id", allocationID),
				slog.String("request_id", RequestID(r.Context())),
			)
			WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		WriteJSON(w, http.StatusOK, ticketResponse{Ticket: ticket, ExpiresAt: expiresAt})
	}
}

// writeAllocationError maps an allocation store error onto the COM-5 envelope. Only
// the four exported sentinels have a client-facing meaning; anything else is logged in
// full and answered with a generic 500 so a database error cannot leak through the
// response.
func writeAllocationError(w http.ResponseWriter, r *http.Request, err error, log *slog.Logger) {
	switch {
	case errors.Is(err, pool.ErrNoCapacity):
		WriteError(w, r, http.StatusServiceUnavailable, "no_capacity", "no game server with enough capacity is available")
	case errors.Is(err, pool.ErrAllocationConflict):
		WriteError(w, r, http.StatusConflict, "allocation_conflict", "party already has a live allocation with a different player set")
	case errors.Is(err, pool.ErrNotFound):
		WriteError(w, r, http.StatusNotFound, "not_found", "allocation not found")
	case errors.Is(err, pool.ErrAllocationEnded):
		WriteError(w, r, http.StatusConflict, "allocation_ended", "allocation is not live")
	default:
		log.ErrorContext(r.Context(), "allocation request failed",
			slog.Any("error", err),
			slog.String("request_id", RequestID(r.Context())),
		)
		WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal error")
	}
}
