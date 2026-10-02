// Friends and blocks (SE-5; design/04-session-minimal.md SES-C1 to SES-C5).
//
// A player is found by display name and discriminator, the "Tanuki#4417" players see.
// discriminator is a JSON number here as in every other Session answer and in the SDK.

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/otomo-live/otomo/services/session/internal/store"
)

// SES-C5: at most FriendRequestLimit friend requests per player per FriendRequestWindow.
const (
	FriendRequestLimit  = 20
	FriendRequestWindow = time.Hour
)

// maxFriendBody caps a friend request body, {"display_name","discriminator"}.
const maxFriendBody = 1 << 10

// FriendStore is the part of the store the friend handlers use. *store.DB implements it.
type FriendStore interface {
	FindPlayer(ctx context.Context, displayName string, discriminator int) (string, error)
	RequestFriend(ctx context.Context, player, target string, max int) (store.Friend, []store.Notice, error)
	AcceptFriend(ctx context.Context, player, requester string, max int) (store.Friend, []store.Notice, error)
	DeclineFriend(ctx context.Context, player, requester string) ([]store.Notice, error)
	RemoveFriend(ctx context.Context, player, other string) ([]store.Notice, error)
	Block(ctx context.Context, player, target string) ([]store.Notice, error)
	Unblock(ctx context.Context, player, target string) error
	ListFriends(ctx context.Context, player string) ([]store.Friend, error)
}

// RateLimiter counts an action per player. *ratelimit.Limiter implements it.
type RateLimiter interface {
	Allow(ctx context.Context, action, player string, limit int, window time.Duration) (time.Duration, error)
}

// friendResponse is one entry of GET /friends, and the answer to a request or accept.
// status is the friend's presence, on accepted friends in GET /friends only; state is
// set on the answer to a request.
type friendResponse struct {
	PlayerID      string     `json:"player_id"`
	DisplayName   string     `json:"display_name"`
	Discriminator int        `json:"discriminator"`
	Since         *time.Time `json:"since,omitempty"`
	Status        string     `json:"status,omitempty"`
	State         string     `json:"state,omitempty"`
}

type friendsResponse struct {
	Friends  []friendResponse `json:"friends"`
	Incoming []friendResponse `json:"incoming"`
	Outgoing []friendResponse `json:"outgoing"`
}

type friendRequest struct {
	DisplayName   string `json:"display_name"`
	Discriminator *int   `json:"discriminator"`
}

func friendFrom(f store.Friend) friendResponse {
	out := friendResponse{PlayerID: f.PlayerID, DisplayName: f.DisplayName, Discriminator: f.Discriminator}
	if !f.Since.IsZero() {
		since := f.Since
		out.Since = &since
	}
	return out
}

func (h *Handlers) friendStoreReady(w http.ResponseWriter, r *http.Request) bool {
	if h.Friends == nil {
		h.internalError(w, r, errors.New("no friend store configured"))
		return false
	}
	return true
}

// writeFriendError maps a store outcome onto its HTTP answer.
func (h *Handlers) writeFriendError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrProfileNotFound):
		writeProfileNotFound(w, r)
	case errors.Is(err, store.ErrPlayerNotFound):
		WriteError(w, r, http.StatusNotFound, "player_not_found", "no such player")
	case errors.Is(err, store.ErrBlocked):
		WriteError(w, r, http.StatusForbidden, "blocked", "you cannot send that player a friend request")
	case errors.Is(err, store.ErrAlreadyFriends):
		WriteError(w, r, http.StatusConflict, "already_friends", "you are already friends")
	case errors.Is(err, store.ErrFriendLimit):
		WriteError(w, r, http.StatusConflict, "friend_limit",
			fmt.Sprintf("a player can have at most %d friends", h.rules().Friends.MaxFriends))
	case errors.Is(err, store.ErrRequestNotFound):
		WriteError(w, r, http.StatusNotFound, "request_not_found", "no friend request from that player")
	case errors.Is(err, store.ErrNotFriends):
		WriteError(w, r, http.StatusNotFound, "not_friends", "you are not friends with that player")
	case errors.Is(err, store.ErrTargetIsYourself):
		WriteError(w, r, http.StatusBadRequest, "invalid_target", "you cannot do that to yourself")
	default:
		h.internalError(w, r, err)
	}
}

// listFriends serves GET /friends: accepted friends with their presence, and requests in
// both directions. One SQL query, then one Valkey pipeline for every friend's presence
// (SES-C4). If presence cannot be read, friends are listed without a status.
func (h *Handlers) listFriends(w http.ResponseWriter, r *http.Request) {
	if !h.friendStoreReady(w, r) {
		return
	}
	me := caller(r)
	all, err := h.Friends.ListFriends(r.Context(), me)
	if err != nil {
		h.writeFriendError(w, r, err)
		return
	}
	out := friendsResponse{Friends: []friendResponse{}, Incoming: []friendResponse{}, Outgoing: []friendResponse{}}
	var ids []string
	for _, f := range all {
		switch {
		case f.State == store.FriendAccepted:
			out.Friends = append(out.Friends, friendFrom(f))
			ids = append(ids, f.PlayerID)
		case f.RequestedBy == me:
			out.Outgoing = append(out.Outgoing, friendFrom(f))
		default:
			out.Incoming = append(out.Incoming, friendFrom(f))
		}
	}
	if h.Presence != nil && len(ids) > 0 {
		statuses, err := h.Presence.Statuses(r.Context(), ids)
		if err != nil {
			if h.Log != nil {
				h.Log.Warn("friend presence unreadable", slog.String("error", err.Error()),
					slog.String("request_id", RequestID(r.Context())))
			}
		} else {
			for i := range out.Friends {
				out.Friends[i].Status = statuses[out.Friends[i].PlayerID]
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// requestFriend serves POST /friends/requests {"display_name","discriminator"}. It
// answers 200 with the player and the friendship's state: pending, or accepted when they
// had already asked the caller.
func (h *Handlers) requestFriend(w http.ResponseWriter, r *http.Request) {
	var req friendRequest
	if !decodeBody(w, r, maxFriendBody, &req) {
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" || req.Discriminator == nil || *req.Discriminator < 1 || *req.Discriminator > 9999 {
		WriteError(w, r, http.StatusBadRequest, "invalid_player_tag",
			"display_name and discriminator (a number from 1 to 9999) are required")
		return
	}
	if !h.friendStoreReady(w, r) {
		return
	}
	me := caller(r)
	if h.Limits != nil {
		wait, err := h.Limits.Allow(r.Context(), "friend_request", me, FriendRequestLimit, FriendRequestWindow)
		if err != nil {
			h.internalError(w, r, err)
			return
		}
		if wait > 0 {
			secs := int64((wait + time.Second - 1) / time.Second)
			w.Header().Set("Retry-After", strconv.FormatInt(max(secs, 1), 10))
			WriteError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded",
				fmt.Sprintf("at most %d friend requests an hour", FriendRequestLimit))
			return
		}
	}
	target, err := h.Friends.FindPlayer(r.Context(), name, *req.Discriminator)
	if err != nil {
		h.writeFriendError(w, r, err)
		return
	}
	them, notices, err := h.Friends.RequestFriend(r.Context(), me, target, h.rules().Friends.MaxFriends)
	if err != nil {
		h.writeFriendError(w, r, err)
		return
	}
	h.publish(r, notices)
	resp := friendFrom(them)
	resp.State = them.State
	writeJSON(w, http.StatusOK, resp)
}

// acceptFriend serves POST /friends/requests/{player_id}/accept.
func (h *Handlers) acceptFriend(w http.ResponseWriter, r *http.Request) {
	requester, ok := pathID(w, r, "player_id", "invalid_player_id")
	if !ok || !h.friendStoreReady(w, r) {
		return
	}
	them, notices, err := h.Friends.AcceptFriend(r.Context(), caller(r), requester, h.rules().Friends.MaxFriends)
	if err != nil {
		h.writeFriendError(w, r, err)
		return
	}
	h.publish(r, notices)
	resp := friendFrom(them)
	resp.State = store.FriendAccepted
	writeJSON(w, http.StatusOK, resp)
}

// declineFriend serves POST /friends/requests/{player_id}/decline.
func (h *Handlers) declineFriend(w http.ResponseWriter, r *http.Request) {
	requester, ok := pathID(w, r, "player_id", "invalid_player_id")
	if !ok || !h.friendStoreReady(w, r) {
		return
	}
	notices, err := h.Friends.DeclineFriend(r.Context(), caller(r), requester)
	if err != nil {
		h.writeFriendError(w, r, err)
		return
	}
	h.publish(r, notices)
	w.WriteHeader(http.StatusNoContent)
}

// removeFriend serves DELETE /friends/{player_id}: ends a friendship, or withdraws or
// refuses a request, in either direction.
func (h *Handlers) removeFriend(w http.ResponseWriter, r *http.Request) {
	other, ok := pathID(w, r, "player_id", "invalid_player_id")
	if !ok || !h.friendStoreReady(w, r) {
		return
	}
	notices, err := h.Friends.RemoveFriend(r.Context(), caller(r), other)
	if err != nil {
		h.writeFriendError(w, r, err)
		return
	}
	h.publish(r, notices)
	w.WriteHeader(http.StatusNoContent)
}

// blockPlayer serves POST /blocks/{player_id}: 204, also when already blocked. The
// friendship and every party invite between the two end in the same transaction.
func (h *Handlers) blockPlayer(w http.ResponseWriter, r *http.Request) {
	target, ok := pathID(w, r, "player_id", "invalid_player_id")
	if !ok || !h.friendStoreReady(w, r) {
		return
	}
	notices, err := h.Friends.Block(r.Context(), caller(r), target)
	if err != nil {
		h.writeFriendError(w, r, err)
		return
	}
	h.publish(r, notices)
	w.WriteHeader(http.StatusNoContent)
}

// unblockPlayer serves DELETE /blocks/{player_id}: 204, also when not blocked.
func (h *Handlers) unblockPlayer(w http.ResponseWriter, r *http.Request) {
	target, ok := pathID(w, r, "player_id", "invalid_player_id")
	if !ok || !h.friendStoreReady(w, r) {
		return
	}
	if err := h.Friends.Unblock(r.Context(), caller(r), target); err != nil {
		h.writeFriendError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
