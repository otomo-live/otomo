// GET /events?after= (SE-4): the long-poll of design/04-session-minimal.md §4,
// with the answers design/12-godot-sdk-guide.md §7.3 gives the client.

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/events"
)

// defaultEventHold is §4's 25 s, used when Handlers.EventHold is not set.
const defaultEventHold = 25 * time.Second

// EventReader is the part of the event client the long-poll uses. *events.Client
// implements it; handler tests supply a fake so the poll can be tested without Valkey.
type EventReader interface {
	Read(ctx context.Context, playerID string, after int64) ([]events.Event, bool, error)
}

// resyncResponse is the answer for a cursor the stream can no longer serve.
type resyncResponse struct {
	Resync bool `json:"resync"`
}

// pollEvents serves GET /events?after=<seq>. It answers at once when there are events
// after the cursor, and otherwise holds the request until one arrives, the hold ends, a
// newer poll by the same player replaces it, or the client goes away.
//
// The waiter is registered before the first read. A publish that lands between the read
// and the wait then leaves a wake-up in the waiter's buffer instead of being missed.
func (h *Handlers) pollEvents(w http.ResponseWriter, r *http.Request) {
	after, ok := parseAfter(r.URL.Query().Get("after"))
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "invalid_after", "after must be a whole number of 0 or more")
		return
	}
	if h.Events == nil || h.Hub == nil {
		h.internalError(w, r, errors.New("no event reader or hub configured"))
		return
	}
	player := auth.IdentityFrom(r.Context()).Subject

	waiter := h.Hub.Register(player)
	defer waiter.Release()

	hold := h.EventHold
	if hold <= 0 {
		hold = defaultEventHold
	}
	timer := time.NewTimer(hold)
	defer timer.Stop()

	for {
		evs, resync, err := h.Events.Read(r.Context(), player, after)
		if err != nil {
			if r.Context().Err() != nil {
				return // the client left; there is nobody to answer
			}
			h.internalError(w, r, err)
			return
		}
		if resync {
			writeJSON(w, http.StatusOK, resyncResponse{Resync: true})
			return
		}
		if len(evs) > 0 {
			writeJSON(w, http.StatusOK, evs)
			return
		}

		select {
		case <-waiter.Wake:
			// Something was published for this player; read again.
		case <-waiter.Done:
			writeJSON(w, http.StatusOK, []events.Event{})
			return
		case <-timer.C:
			writeJSON(w, http.StatusOK, []events.Event{})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// parseAfter reads the cursor. A missing value is 0, which is where a client starts
// after login.
func parseAfter(raw string) (int64, bool) {
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
