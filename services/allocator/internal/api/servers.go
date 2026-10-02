// Game-server registry routes: the endpoints a game server calls to join the pool and
// stay in it, plus the pool read the Gameplay Proxy uses to find where a ticket's
// server lives. The game-server routes authenticate with the gameserver service key;
// the read is behind the proxy's own key.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/otomo-live/otomo/services/allocator/internal/pool"
	"github.com/otomo-live/otomo/services/allocator/internal/servicekey"
)

// maxRequestBodyBytes bounds a registry request body at 4 KiB. Every body on these
// routes is a handful of scalars, so a larger one is a client bug or an attempt to make
// the decoder do work; MaxBytesReader turns it into a clean 400 instead of reading
// whatever the caller sends.
const maxRequestBodyBytes = 4 << 10

// ServerStore is the slice of the registry the HTTP layer needs. It is an interface so
// the request/response mapping can be tested with a fake and the registry can be tested
// without a listener; pool.Pool satisfies it.
type ServerStore interface {
	Register(ctx context.Context, reg pool.Registration) error
	Heartbeat(ctx context.Context, serverID string, playersConnected int) (*pool.Assignment, error)
	Ended(ctx context.Context, serverID, allocationID string) error
	ListServers(ctx context.Context) ([]pool.ServerInfo, error)
}

// pool.Pool is the production implementation. The assertion keeps the interface and
// the registry from drifting apart without waiting for the wiring in main to fail.
var _ ServerStore = (*pool.Pool)(nil)

// heartbeatResponse is the heartbeat body. Allocation is a pointer so a server with no
// hold renders as an explicit `"allocation": null` rather than an omitted key: a caller
// checks that field either way, and "absent" would be a third state to handle.
type heartbeatResponse struct {
	Allocation *pool.Assignment `json:"allocation"`
}

// serverListResponse is the pool read the Gameplay Proxy gets. Servers is a slice so an
// empty pool renders as `{"servers":[]}`, never null: the proxy iterates it and should
// not have to treat "no servers" as a missing key.
type serverListResponse struct {
	Servers []pool.ServerInfo `json:"servers"`
}

// RegisterServerRoutes mounts the three registry routes on mux, each behind the
// gameserver key, and the pool read behind the proxy key.
//
// M1 ships one shared gameserver key, so Require cannot tell game servers apart: any
// server holding the secret may register, heartbeat for, or end the allocation of any
// server_id. That is a deliberate M1 simplification — per-server credentials are a
// later ticket — and it is one more reason these routes are internal-only.
func RegisterServerRoutes(mux *http.ServeMux, keys *servicekey.Keys, store ServerStore) {
	mux.Handle("POST /internal/servers/register",
		keys.Require(servicekey.RoleGameServer, handleRegisterServer(store)))
	mux.Handle("POST /internal/servers/{server_id}/heartbeat",
		keys.Require(servicekey.RoleGameServer, handleHeartbeat(store)))
	mux.Handle("POST /internal/servers/{server_id}/ended",
		keys.Require(servicekey.RoleGameServer, handleEnded(store)))
	// The Gameplay Proxy reads the pool here: a ticket's srv claim names a server, and
	// this is where the proxy learns that server's internal_addr. It is a separate role
	// from the game servers so a proxy key can read the pool but not register into it,
	// and a game server cannot enumerate its peers.
	mux.Handle("GET /internal/servers",
		keys.Require(servicekey.RoleProxy, handleListServers(store)))
}

// handleRegisterServer decodes and validates a registration, then hands it to the
// store. The store re-validates, so a direct caller cannot skip the checks.
func handleRegisterServer(store ServerStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var reg pool.Registration
		if !decodeBody(w, r, &reg) {
			return
		}
		if err := pool.ValidateRegistration(reg); err != nil {
			WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err := store.Register(r.Context(), reg); err != nil {
			writeStoreError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleHeartbeat records liveness and returns the server's current allocation. The
// server id comes from the path, so the body carries only the player count.
func handleHeartbeat(store ServerStore) http.HandlerFunc {
	type request struct {
		PlayersConnected int `json:"players_connected"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if !decodeBody(w, r, &req) {
			return
		}
		if err := pool.ValidatePlayersConnected(req.PlayersConnected); err != nil {
			WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		assignment, err := store.Heartbeat(r.Context(), r.PathValue("server_id"), req.PlayersConnected)
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, heartbeatResponse{Allocation: assignment})
	}
}

// handleEnded reports that an allocation is over. The allocation id is validated here
// so a non-UUID becomes a 400 with a named field instead of reaching Postgres's uuid
// parser as an internal error.
func handleEnded(store ServerStore) http.HandlerFunc {
	type request struct {
		AllocationID string `json:"allocation_id"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if !decodeBody(w, r, &req) {
			return
		}
		if err := pool.ValidateAllocationID(req.AllocationID); err != nil {
			WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err := store.Ended(r.Context(), r.PathValue("server_id"), req.AllocationID); err != nil {
			writeStoreError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleListServers answers the Gameplay Proxy with every live server. The store
// already omits dead servers and orders the result, and a store error goes through the
// existing helper, so an empty pool is the only shape handled here: a nil slice is
// replaced with an empty one so the body is always a JSON array.
func handleListServers(store ServerStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		servers, err := store.ListServers(r.Context())
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		if servers == nil {
			servers = []pool.ServerInfo{}
		}
		WriteJSON(w, http.StatusOK, serverListResponse{Servers: servers})
	}
}

// decodeBody reads one JSON object from the request into dst, answering 400 in the
// COM-5 shape when it cannot. Unknown fields are rejected so a client that misspells a
// field learns about it instead of having it silently ignored.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_request", decodeMessage(err))
		return false
	}
	// A second value is not a request. Rejecting it stops a caller from smuggling an
	// object past a handler that only reads the first one.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		WriteError(w, r, http.StatusBadRequest, "invalid_request", "request body must contain a single JSON object")
		return false
	}
	return true
}

// decodeMessage turns a decoder error into a message for the caller. JSON's own errors
// already name the offending field, which is what the contract asks for; only the two
// cases with no field to name — an empty body and one over the size cap — are
// rewritten.
func decodeMessage(err error) string {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, io.EOF):
		return "request body is empty"
	case errors.As(err, &tooLarge):
		return fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit)
	default:
		return err.Error()
	}
}

// writeStoreError maps a registry error onto the COM-5 envelope. Only the two exported
// sentinels have a client-facing meaning; anything else is logged in full and answered
// with a generic 500 so a database error cannot leak through the response.
func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, pool.ErrNotRegistered):
		WriteError(w, r, http.StatusNotFound, "not_registered", "game server is not registered")
	case errors.Is(err, pool.ErrAllocationMismatch):
		WriteError(w, r, http.StatusConflict, "allocation_mismatch", "allocation does not belong to this server")
	default:
		slog.ErrorContext(r.Context(), "game server registry request failed",
			slog.Any("error", err),
			slog.String("request_id", RequestID(r.Context())),
		)
		WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal error")
	}
}
