// Presence (SE-3): POST /presence/heartbeat, and member presence on
// GET /party.

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/otomo-live/otomo/services/session/internal/presence"
)

// maxHeartbeatBody caps a heartbeat body, {"status"}.
const maxHeartbeatBody = 1 << 8

// PresenceService is what the handlers need from presence. *presence.Service
// implements it.
type PresenceService interface {
	Heartbeat(ctx context.Context, player, status string) (presence.Beat, error)
	Statuses(ctx context.Context, players []string) (map[string]string, error)
}

type heartbeatRequest struct {
	Status string `json:"status"`
}

// heartbeat serves POST /presence/heartbeat {"status"}: status is online, in_menus or
// away. It answers 204, or 429 rate_limit_exceeded with Retry-After when the caller's
// last beat was under 10 s ago. A player who stops beating is offline 60 s after the
// last beat, with nothing to send on quit.
func (h *Handlers) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatRequest
	if !decodeBody(w, r, maxHeartbeatBody, &req) {
		return
	}
	if !presence.ValidStatus(req.Status) {
		WriteError(w, r, http.StatusBadRequest, "invalid_status", `status must be "online", "in_menus" or "away"`)
		return
	}
	if h.Presence == nil {
		h.internalError(w, r, errors.New("no presence service configured"))
		return
	}
	beat, err := h.Presence.Heartbeat(r.Context(), caller(r), req.Status)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	if beat.RetryAfter > 0 {
		secs := int64((beat.RetryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(max(secs, 1), 10))
		WriteError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded", "at most one heartbeat every 10 seconds")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// withStatuses fills in each member's presence status on a GET /party answer. Presence is
// a hint: when it cannot be read the party is still answered, without statuses.
func (h *Handlers) withStatuses(r *http.Request, out *partyResponse) {
	if h.Presence == nil || len(out.Members) == 0 {
		return
	}
	ids := make([]string, len(out.Members))
	for i, m := range out.Members {
		ids[i] = m.PlayerID
	}
	statuses, err := h.Presence.Statuses(r.Context(), ids)
	if err != nil {
		if h.Log != nil {
			h.Log.Warn("party member presence unreadable", slog.String("error", err.Error()),
				slog.String("request_id", RequestID(r.Context())))
		}
		return
	}
	for i := range out.Members {
		out.Members[i].Status = statuses[out.Members[i].PlayerID]
	}
}
