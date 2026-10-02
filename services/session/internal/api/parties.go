// Party routes (SE-6): create, get, invite, accept, decline, leave, kick and
// promote.
//
// Who may do what:
//   - any member may invite (design/14-launch-handoff.md §2 lists invite as a member action,
//     and design/04-session-minimal.md §5 left it open);
//   - kick and promote are leader calls. Like every leader call they carry the revision
//     the leader acted on, {"revision": n}, and a stale one is refused with
//     409 revision_mismatch so a leader never acts on a party they have not seen.
//
// Events are published only after the store call has returned, which is after its
// transaction committed: a change that rolled back notifies nobody.

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/allocator"
	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/events"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

// maxPartyBody caps a party request body. The largest is one player id.
const maxPartyBody = 1 << 10

// publishTimeout bounds publishing a change's events. It runs on a context detached from
// the request, so a client that hangs up after its change committed still leaves the
// other members notified.
const publishTimeout = 5 * time.Second

// PartyStore is the part of the store the party handlers use. *store.DB implements it.
type PartyStore interface {
	CreateParty(ctx context.Context, player string, maxSize int, settings map[string]string) (*store.Party, []store.Notice, error)
	GetParty(ctx context.Context, player string) (*store.Party, error)
	InviteToParty(ctx context.Context, player, target string, maxSize int) (*store.Invite, []store.Notice, error)
	AcceptInvite(ctx context.Context, player, inviteID string, maxSize int) (*store.Party, []store.Notice, error)
	DeclineInvite(ctx context.Context, player, inviteID string) error
	LeaveParty(ctx context.Context, player string) (*store.Party, []store.Notice, error)
	KickFromParty(ctx context.Context, leader, target string, revision int) (*store.Party, []store.Notice, error)
	PromoteInParty(ctx context.Context, leader, target string, revision int) (*store.Party, []store.Notice, error)
	UpdatePartySettings(ctx context.Context, leader string, changes map[string]string, revision int) (*store.Party, []store.Notice, error)
	SetReady(ctx context.Context, player string, ready bool) (*store.Party, []store.Notice, error)
	StartLaunch(ctx context.Context, leader string, revision int, check func(map[string]string) error) (*store.Party, []store.Notice, []string, bool, error)
}

// Publisher sends one event to a player's stream. *events.Client implements it.
type Publisher interface {
	Publish(ctx context.Context, playerID, typ string, payload any) (events.Event, error)
}

type partyMember struct {
	PlayerID      string    `json:"player_id"`
	DisplayName   string    `json:"display_name"`
	Discriminator int       `json:"discriminator"`
	JoinedAt      time.Time `json:"joined_at"`
	Ready         bool      `json:"ready"`
	// Status is the member's presence (SE-3): online, in_menus, away or offline. Only
	// GET /party fills it in.
	Status string `json:"status,omitempty"`
}

// partyResponse is GET /party's body and the answer to every change that leaves the
// caller in a party. max_size is the limit in force now, from the rules. state and
// settings are the lobby's (design/14-launch-handoff.md §2.1).
type partyResponse struct {
	PartyID  string            `json:"party_id"`
	LeaderID string            `json:"leader_id"`
	Revision int               `json:"revision"`
	MaxSize  int               `json:"max_size"`
	State    string            `json:"state"`
	Settings map[string]string `json:"settings"`
	Members  []partyMember     `json:"members"`
	Match    *matchResponse    `json:"match,omitempty"` // only while in_game
}

type settingsRequest struct {
	Settings map[string]string `json:"settings"`
	Revision *int              `json:"revision"`
}

type readyRequest struct {
	Ready *bool `json:"ready"`
}

type inviteResponse struct {
	InviteID  string    `json:"invite_id"`
	PartyID   string    `json:"party_id"`
	ToPlayer  string    `json:"to_player"`
	ExpiresAt time.Time `json:"expires_at"`
}

type inviteRequest struct {
	PlayerID string `json:"player_id"`
}

type revisionRequest struct {
	Revision *int `json:"revision"`
}

func (h *Handlers) partyFrom(p *store.Party) partyResponse {
	out := partyResponse{
		PartyID:  p.ID,
		LeaderID: p.LeaderID,
		Revision: p.Revision,
		MaxSize:  h.rules().Party.MaxSize,
		State:    p.State,
		Settings: p.Settings,
		Members:  make([]partyMember, 0, len(p.Members)),
	}
	if out.Settings == nil {
		out.Settings = map[string]string{}
	}
	if p.State == store.StateInGame && p.AllocationID != "" {
		out.Match = &matchResponse{AllocationID: p.AllocationID, Address: p.MatchAddress, Port: p.MatchPort}
	}
	for _, m := range p.Members {
		out.Members = append(out.Members, partyMember{
			PlayerID: m.PlayerID, DisplayName: m.DisplayName,
			Discriminator: m.Discriminator, JoinedAt: m.JoinedAt, Ready: m.Ready,
		})
	}
	return out
}

// defaultSettings returns every lobby setting at its default: a new lobby's settings.
func (h *Handlers) defaultSettings() map[string]string {
	out := map[string]string{}
	for k, s := range h.rules().Lobby.Settings {
		out[k] = s.Default
	}
	return out
}

// checkSettings applies design/14-launch-handoff.md §2: every key must be a listed
// lobby setting and every value one of its allowed values. It returns a message for
// the 400, or "" when the settings are acceptable.
func (h *Handlers) checkSettings(changes map[string]string) string {
	listed := h.rules().Lobby.Settings
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		s, ok := listed[k]
		if !ok {
			return fmt.Sprintf("%q is not a lobby setting", k)
		}
		if !slices.Contains(s.Allowed, changes[k]) {
			return fmt.Sprintf("%q is not an allowed value for %s", changes[k], k)
		}
	}
	return ""
}

// publish sends a committed change's notices. A failure is logged and not returned:
// events are hints, the change is already committed, and the client that made it has
// its answer.
func (h *Handlers) publish(r *http.Request, notices []store.Notice) {
	if len(notices) == 0 || h.Publisher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), publishTimeout)
	defer cancel()
	for _, n := range notices {
		if _, err := h.Publisher.Publish(ctx, n.PlayerID, n.Type, n.Payload); err != nil && h.Log != nil {
			h.Log.LogAttrs(ctx, slog.LevelWarn, "event not published",
				slog.String("type", n.Type),
				slog.String("error", err.Error()),
				slog.String("request_id", RequestID(r.Context())),
			)
		}
	}
}

// partyStoreReady answers 500 when no party store is wired.
func (h *Handlers) partyStoreReady(w http.ResponseWriter, r *http.Request) bool {
	if h.Parties == nil {
		h.internalError(w, r, errors.New("no party store configured"))
		return false
	}
	return true
}

func caller(r *http.Request) string {
	return auth.IdentityFrom(r.Context()).PlayerUUID().String()
}

// pathID reads a UUID path value. On a malformed one it writes the 400 and returns false.
func pathID(w http.ResponseWriter, r *http.Request, name, code string) (string, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, code, name+" must be a UUID")
		return "", false
	}
	return id.String(), true
}

// writePartyError maps a store outcome onto its HTTP answer.
func (h *Handlers) writePartyError(w http.ResponseWriter, r *http.Request, err error) {
	var stale *store.RevisionMismatchError
	switch {
	case errors.As(err, &stale):
		WriteError(w, r, http.StatusConflict, "revision_mismatch", "the party has changed; fetch it and try again")
	case errors.Is(err, store.ErrNotInParty):
		WriteError(w, r, http.StatusNotFound, "not_in_party", "you are not in a party")
	case errors.Is(err, store.ErrAlreadyInParty):
		WriteError(w, r, http.StatusConflict, "already_in_party", "you are already in a party; leave it first")
	case errors.Is(err, store.ErrNotLeader):
		WriteError(w, r, http.StatusForbidden, "not_leader", "only the party leader can do that")
	case errors.Is(err, store.ErrPartyFull):
		WriteError(w, r, http.StatusConflict, "party_full", "the party is full")
	case errors.Is(err, store.ErrInviteNotFound):
		WriteError(w, r, http.StatusNotFound, "invite_not_found", "no such invite")
	case errors.Is(err, store.ErrInviteExpired):
		WriteError(w, r, http.StatusGone, "invite_expired", "the invite has expired")
	case errors.Is(err, store.ErrNotAMember):
		WriteError(w, r, http.StatusNotFound, "not_a_member", "that player is not in your party")
	case errors.Is(err, store.ErrAlreadyMember):
		WriteError(w, r, http.StatusConflict, "already_member", "that player is already in your party")
	case errors.Is(err, store.ErrPlayerNotFound):
		WriteError(w, r, http.StatusNotFound, "player_not_found", "no such player")
	case errors.Is(err, store.ErrBlocked):
		WriteError(w, r, http.StatusForbidden, "blocked", "you cannot invite that player")
	case errors.Is(err, store.ErrPartyLocked):
		WriteError(w, r, http.StatusConflict, "party_locked", "the party is launching or in a match; only leaving is allowed")
	case errors.Is(err, store.ErrTargetIsYourself):
		WriteError(w, r, http.StatusBadRequest, "invalid_target", "you cannot do that to yourself")
	default:
		h.internalError(w, r, err)
	}
}

// createParty serves POST /party.
func (h *Handlers) createParty(w http.ResponseWriter, r *http.Request) {
	if !h.partyStoreReady(w, r) {
		return
	}
	p, notices, err := h.Parties.CreateParty(r.Context(), caller(r), h.rules().Party.MaxSize, h.defaultSettings())
	if err != nil {
		h.writePartyError(w, r, err)
		return
	}
	h.publish(r, notices)
	writeJSON(w, http.StatusCreated, h.partyFrom(p))
}

// getParty serves GET /party.
func (h *Handlers) getParty(w http.ResponseWriter, r *http.Request) {
	if !h.partyStoreReady(w, r) {
		return
	}
	p, err := h.Parties.GetParty(r.Context(), caller(r))
	if err != nil {
		h.writePartyError(w, r, err)
		return
	}
	out := h.partyFrom(p)
	h.withStatuses(r, &out)
	writeJSON(w, http.StatusOK, out)
}

// inviteToParty serves POST /party/invites {"player_id"}.
func (h *Handlers) inviteToParty(w http.ResponseWriter, r *http.Request) {
	var req inviteRequest
	if !decodeBody(w, r, maxPartyBody, &req) {
		return
	}
	target, err := uuid.Parse(req.PlayerID)
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_player_id", "player_id must be a UUID")
		return
	}
	if !h.partyStoreReady(w, r) {
		return
	}
	inv, notices, err := h.Parties.InviteToParty(r.Context(), caller(r), target.String(), h.rules().Party.MaxSize)
	if err != nil {
		h.writePartyError(w, r, err)
		return
	}
	h.publish(r, notices)
	writeJSON(w, http.StatusCreated, inviteResponse{
		InviteID: inv.ID, PartyID: inv.PartyID, ToPlayer: inv.ToPlayer, ExpiresAt: inv.ExpiresAt,
	})
}

// acceptInvite serves POST /party/invites/{invite_id}/accept.
func (h *Handlers) acceptInvite(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "invite_id", "invalid_invite_id")
	if !ok || !h.partyStoreReady(w, r) {
		return
	}
	p, notices, err := h.Parties.AcceptInvite(r.Context(), caller(r), id, h.rules().Party.MaxSize)
	if err != nil {
		h.writePartyError(w, r, err)
		return
	}
	h.publish(r, notices)
	writeJSON(w, http.StatusOK, h.partyFrom(p))
}

// declineInvite serves POST /party/invites/{invite_id}/decline.
func (h *Handlers) declineInvite(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "invite_id", "invalid_invite_id")
	if !ok || !h.partyStoreReady(w, r) {
		return
	}
	if err := h.Parties.DeclineInvite(r.Context(), caller(r), id); err != nil {
		h.writePartyError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// leaveParty serves POST /party/leave.
func (h *Handlers) leaveParty(w http.ResponseWriter, r *http.Request) {
	if !h.partyStoreReady(w, r) {
		return
	}
	_, notices, err := h.Parties.LeaveParty(r.Context(), caller(r))
	if err != nil {
		h.writePartyError(w, r, err)
		return
	}
	h.publish(r, notices)
	w.WriteHeader(http.StatusNoContent)
}

// leaderCall reads the target from the path and the revision from the body of a leader
// call. On a bad request it writes the 400 and returns false.
func leaderCall(w http.ResponseWriter, r *http.Request) (target string, revision int, ok bool) {
	target, ok = pathID(w, r, "player_id", "invalid_player_id")
	if !ok {
		return "", 0, false
	}
	var req revisionRequest
	if !decodeBody(w, r, maxPartyBody, &req) {
		return "", 0, false
	}
	if req.Revision == nil {
		WriteError(w, r, http.StatusBadRequest, "revision_required",
			`a leader call must carry the party revision it acts on: {"revision": n}`)
		return "", 0, false
	}
	return target, *req.Revision, true
}

// kickFromParty serves POST /party/kick/{player_id} {"revision"}.
func (h *Handlers) kickFromParty(w http.ResponseWriter, r *http.Request) {
	target, revision, ok := leaderCall(w, r)
	if !ok || !h.partyStoreReady(w, r) {
		return
	}
	p, notices, err := h.Parties.KickFromParty(r.Context(), caller(r), target, revision)
	if err != nil {
		h.writePartyError(w, r, err)
		return
	}
	h.publish(r, notices)
	writeJSON(w, http.StatusOK, h.partyFrom(p))
}

// promoteInParty serves POST /party/promote/{player_id} {"revision"}.
func (h *Handlers) promoteInParty(w http.ResponseWriter, r *http.Request) {
	target, revision, ok := leaderCall(w, r)
	if !ok || !h.partyStoreReady(w, r) {
		return
	}
	p, notices, err := h.Parties.PromoteInParty(r.Context(), caller(r), target, revision)
	if err != nil {
		h.writePartyError(w, r, err)
		return
	}
	h.publish(r, notices)
	writeJSON(w, http.StatusOK, h.partyFrom(p))
}

// updateSettings serves PATCH /party/settings {"settings":{...},"revision":n}
// (design/14-launch-handoff.md §2.1): leader only, in forming, at the current revision.
// Keys the request leaves out keep their values; every key given must be a listed lobby
// setting with an allowed value (400 invalid_settings). A change clears every ready flag.
func (h *Handlers) updateSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if !decodeBody(w, r, maxPartyBody, &req) {
		return
	}
	if req.Revision == nil {
		WriteError(w, r, http.StatusBadRequest, "revision_required",
			`a leader call must carry the party revision it acts on: {"revision": n}`)
		return
	}
	if req.Settings == nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_settings", "settings is required")
		return
	}
	if msg := h.checkSettings(req.Settings); msg != "" {
		WriteError(w, r, http.StatusBadRequest, "invalid_settings", msg)
		return
	}
	if !h.partyStoreReady(w, r) {
		return
	}
	p, notices, err := h.Parties.UpdatePartySettings(r.Context(), caller(r), req.Settings, *req.Revision)
	if err != nil {
		h.writePartyError(w, r, err)
		return
	}
	h.publish(r, notices)
	writeJSON(w, http.StatusOK, h.partyFrom(p))
}

// setReady serves POST /party/ready {"ready":true|false}: any member, in forming.
func (h *Handlers) setReady(w http.ResponseWriter, r *http.Request) {
	var req readyRequest
	if !decodeBody(w, r, maxPartyBody, &req) {
		return
	}
	if req.Ready == nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", `the body must be {"ready": true} or {"ready": false}`)
		return
	}
	if !h.partyStoreReady(w, r) {
		return
	}
	p, notices, err := h.Parties.SetReady(r.Context(), caller(r), *req.Ready)
	if err != nil {
		h.writePartyError(w, r, err)
		return
	}
	h.publish(r, notices)
	writeJSON(w, http.StatusOK, h.partyFrom(p))
}

// Launcher finishes a launch in the background once launching is committed.
// *launch.Launcher implements it.
type Launcher interface {
	Start(partyID string, members []string)
}

// TicketIssuer issues a fresh join ticket. *allocator.Client implements it.
type TicketIssuer interface {
	Ticket(ctx context.Context, allocationID, playerID string) (string, time.Time, error)
}

// matchResponse is GET /party's match while in_game: where to connect, never a ticket.
type matchResponse struct {
	AllocationID string `json:"allocation_id"`
	Address      string `json:"address"`
	Port         int    `json:"port"`
}

type ticketResponse struct {
	Address         string    `json:"address"`
	Port            int       `json:"port"`
	Ticket          string    `json:"ticket"`
	TicketExpiresAt time.Time `json:"ticket_expires_at"`
}

// launchParty serves POST /party/launch {"revision":n} (design/14-launch-handoff.md §2.1,
// §3): leader, every member ready, forming. It commits launching and answers 202; the
// Allocator is asked afterwards, outside the transaction, and the outcome reaches the
// members as party.launching or party.launch_failed. A press while already launching
// answers 202 again and starts nothing.
func (h *Handlers) launchParty(w http.ResponseWriter, r *http.Request) {
	var req revisionRequest
	if !decodeBody(w, r, maxPartyBody, &req) {
		return
	}
	if req.Revision == nil {
		WriteError(w, r, http.StatusBadRequest, "revision_required",
			`a leader call must carry the party revision it acts on: {"revision": n}`)
		return
	}
	if !h.partyStoreReady(w, r) {
		return
	}
	if h.Launcher == nil {
		WriteError(w, r, http.StatusServiceUnavailable, "launch_unavailable", "launching is not configured on this server")
		return
	}

	check := func(settings map[string]string) error {
		if msg := h.checkSettings(settings); msg != "" {
			return errors.New(msg)
		}
		return nil
	}
	p, notices, members, started, err := h.Parties.StartLaunch(r.Context(), caller(r), *req.Revision, check)
	var badSettings *store.InvalidSettingsError
	switch {
	case errors.Is(err, store.ErrNotReady):
		WriteError(w, r, http.StatusConflict, "not_ready", "every member must be ready to launch")
		return
	case errors.As(err, &badSettings):
		WriteError(w, r, http.StatusConflict, "invalid_settings",
			"the lobby settings are no longer allowed ("+badSettings.Reason+"); change them first")
		return
	case err != nil:
		h.writePartyError(w, r, err)
		return
	}
	h.publish(r, notices)
	if started {
		h.Launcher.Start(p.ID, members)
	}
	writeJSON(w, http.StatusAccepted, h.partyFrom(p))
}

// launchTicket serves POST /party/launch/ticket: a fresh join ticket for the caller's
// match while in_game, for a client that missed party.launching, came back after a
// resync, crashed or held an expired ticket. Session never stores tickets.
func (h *Handlers) launchTicket(w http.ResponseWriter, r *http.Request) {
	if !h.partyStoreReady(w, r) {
		return
	}
	if h.Tickets == nil {
		WriteError(w, r, http.StatusServiceUnavailable, "launch_unavailable", "launching is not configured on this server")
		return
	}
	me := caller(r)
	p, err := h.Parties.GetParty(r.Context(), me)
	if err != nil {
		h.writePartyError(w, r, err)
		return
	}
	if p.State != store.StateInGame || p.AllocationID == "" {
		WriteError(w, r, http.StatusConflict, "not_in_game", "the party is not in a match")
		return
	}
	ticket, expires, err := h.Tickets.Ticket(r.Context(), p.AllocationID, me)
	switch {
	case errors.Is(err, allocator.ErrAllocationEnded):
		WriteError(w, r, http.StatusConflict, "match_ended", "the match has ended")
		return
	case errors.Is(err, allocator.ErrNotFound):
		WriteError(w, r, http.StatusNotFound, "not_in_match", "you are not a player in this match")
		return
	case err != nil:
		if h.Log != nil {
			h.Log.Warn("ticket not issued", slog.String("error", err.Error()), slog.String("request_id", RequestID(r.Context())))
		}
		WriteError(w, r, http.StatusServiceUnavailable, "allocator_unavailable", "the game server allocator did not answer; try again")
		return
	}
	writeJSON(w, http.StatusOK, ticketResponse{Address: p.MatchAddress, Port: p.MatchPort, Ticket: ticket, TicketExpiresAt: expires})
}
