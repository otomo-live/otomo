// Handlers is the seam between the route table and the code behind each route, the same
// shape as Config's.
//
// Routes() states which paths exist and which identity domain may call them; Handlers
// states which of them are implemented. A route with no entry here stays behind
// NotImplemented, so the 501 placeholders shrink one ticket at a time without the route
// table, or the boundaries it encodes, moving.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/events"
	"github.com/otomo-live/otomo/services/session/internal/rules"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

// ProfileStore is the part of the store the profile handlers use. *store.DB implements
// it; handler tests supply a fake so the status mapping can be checked without Postgres.
type ProfileStore interface {
	InitProfile(ctx context.Context, playerID uuid.UUID, provisional func() string) (store.Profile, bool, error)
	GetProfile(ctx context.Context, playerID uuid.UUID) (store.Profile, error)
	RenameProfile(ctx context.Context, playerID uuid.UUID, name string, cooldown time.Duration) (store.Profile, error)
}

// Handlers carries everything a route handler needs from outside this package. A nil
// field leaves the routes that need it failing closed with a 500, never panicking.
type Handlers struct {
	Profiles ProfileStore
	Log      *slog.Logger

	// Rules is where handlers read the game rules on each request. Nil means the
	// compiled-in defaults.
	Rules rules.Source

	// Events and Hub serve the long-poll: Events reads a player's stream and Hub holds
	// this instance's waiting polls. EventHold is how long a poll waits; zero means 25 s.
	Events    EventReader
	Hub       *events.Hub
	EventHold time.Duration

	// Parties is the party store, and Publisher sends the events a committed party
	// change owes. A nil Publisher publishes nothing.
	Parties   PartyStore
	Publisher Publisher

	// Launcher finishes a lobby launch after it is committed, and Tickets issues fresh
	// join tickets (LB-3). Nil leaves both launch routes at 503 launch_unavailable.
	Launcher Launcher
	Tickets  TicketIssuer

	// Admin is the store behind the staff routes (SE-7).
	Admin AdminStore

	// Friends is the friend and block store (SE-5), and Limits counts friend requests
	// (SES-C5). A nil Limits applies no limit.
	Friends FriendStore
	Limits  RateLimiter

	// Presence records heartbeats and reads member presence (SE-3). Nil answers the
	// heartbeat 500 and leaves GET /party without statuses.
	Presence PresenceService

	// Returns brings a lobby back from its match when the Allocator's callback says the
	// allocation ended (LB-4). Nil answers the callback 500, so the Allocator retries.
	Returns Returner
}

// For returns the handler for rt's pattern, or NotImplemented when the pattern has no
// implementation yet. A nil *Handlers is valid and returns NotImplemented for every
// route, which keeps the server tests that assert the 501 placeholders working.
func (h *Handlers) For(rt Route) http.Handler {
	if h == nil {
		return http.HandlerFunc(NotImplemented)
	}
	if handler, ok := h.implemented()[rt.Pattern()]; ok {
		return handler
	}
	return http.HandlerFunc(NotImplemented)
}

// implemented maps a ServeMux pattern to its handler. For is called once per route while
// the mux is built, so building the map on each call costs nothing that matters.
func (h *Handlers) implemented() map[string]http.Handler {
	return map[string]http.Handler{
		http.MethodPost + " " + PlayerPrefix + "/me/init": http.HandlerFunc(h.initProfile),
		http.MethodGet + " " + PlayerPrefix + "/me":       http.HandlerFunc(h.getProfile),
		http.MethodPatch + " " + PlayerPrefix + "/me":     http.HandlerFunc(h.renameProfile),

		http.MethodGet + " " + PlayerPrefix + "/events": http.HandlerFunc(h.pollEvents),

		http.MethodPost + " " + PlayerPrefix + "/presence/heartbeat": http.HandlerFunc(h.heartbeat),

		http.MethodGet + " " + PlayerPrefix + "/friends":                               http.HandlerFunc(h.listFriends),
		http.MethodPost + " " + PlayerPrefix + "/friends/requests":                     http.HandlerFunc(h.requestFriend),
		http.MethodPost + " " + PlayerPrefix + "/friends/requests/{player_id}/accept":  http.HandlerFunc(h.acceptFriend),
		http.MethodPost + " " + PlayerPrefix + "/friends/requests/{player_id}/decline": http.HandlerFunc(h.declineFriend),
		http.MethodDelete + " " + PlayerPrefix + "/friends/{player_id}":                http.HandlerFunc(h.removeFriend),
		http.MethodPost + " " + PlayerPrefix + "/blocks/{player_id}":                   http.HandlerFunc(h.blockPlayer),
		http.MethodDelete + " " + PlayerPrefix + "/blocks/{player_id}":                 http.HandlerFunc(h.unblockPlayer),

		http.MethodGet + " " + StaffPrefix + "/players":                     http.HandlerFunc(h.lookupPlayers),
		http.MethodGet + " " + StaffPrefix + "/players/{id}":                http.HandlerFunc(h.getPlayer),
		http.MethodPost + " " + StaffPrefix + "/parties/{party_id}/disband": http.HandlerFunc(h.forceDisband),
		http.MethodGet + " " + StaffPrefix + "/audit":                       http.HandlerFunc(h.listAudit),

		http.MethodPost + " " + PlayerPrefix + "/party":                             http.HandlerFunc(h.createParty),
		http.MethodGet + " " + PlayerPrefix + "/party":                              http.HandlerFunc(h.getParty),
		http.MethodPost + " " + PlayerPrefix + "/party/invites":                     http.HandlerFunc(h.inviteToParty),
		http.MethodPost + " " + PlayerPrefix + "/party/invites/{invite_id}/accept":  http.HandlerFunc(h.acceptInvite),
		http.MethodPost + " " + PlayerPrefix + "/party/invites/{invite_id}/decline": http.HandlerFunc(h.declineInvite),
		http.MethodPost + " " + PlayerPrefix + "/party/leave":                       http.HandlerFunc(h.leaveParty),
		http.MethodPost + " " + PlayerPrefix + "/party/kick/{player_id}":            http.HandlerFunc(h.kickFromParty),
		http.MethodPost + " " + PlayerPrefix + "/party/promote/{player_id}":         http.HandlerFunc(h.promoteInParty),
		http.MethodPatch + " " + PlayerPrefix + "/party/settings":                   http.HandlerFunc(h.updateSettings),
		http.MethodPost + " " + PlayerPrefix + "/party/ready":                       http.HandlerFunc(h.setReady),
		http.MethodPost + " " + PlayerPrefix + "/party/launch":                      http.HandlerFunc(h.launchParty),
		http.MethodPost + " " + PlayerPrefix + "/party/launch/ticket":               http.HandlerFunc(h.launchTicket),
	}
}

// rules returns the rules in force for this request.
func (h *Handlers) rules() *rules.Rules {
	if h.Rules == nil {
		return rules.Static{}.Current()
	}
	return h.Rules.Current()
}

// internalError renders the one 500 shape and logs the cause with the request ID. The
// error itself never reaches the client: a database error can name a table or a row.
func (h *Handlers) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if h.Log != nil {
		h.Log.LogAttrs(r.Context(), slog.LevelError, "session request failed",
			slog.String("error", err.Error()),
			slog.String("request_id", RequestID(r.Context())),
		)
	}
	WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
}

// decodeBody reads one JSON object of at most max bytes into v. Unknown fields and
// trailing data are refused. On failure it has already written the 400 and returns false.
func decodeBody(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			WriteError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return false
		}
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body must contain a single JSON object")
		return false
	}
	return true
}

// writeJSON writes v as the response body. It is the success-path counterpart to
// WriteError and, like it, takes exclusive ownership of the response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
