package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"uuid"
)

// AllocationEndedPattern is the Allocator's callback route on the internal listener
// (design/14-launch-handoff.md §4.4). It is not in Routes(): that table is the public
// listener's, and this route must never be reachable through a gateway.
const AllocationEndedPattern = "POST /internal/session/allocations/{allocation_id}/ended"

// maxCallbackBody bounds the callback body, {"party_id","reason"}.
const maxCallbackBody = 1 << 10

// Returner puts a party whose match ended back into its lobby. *launch.Returner
// implements it.
type Returner interface {
	Return(ctx context.Context, partyID, allocationID, reason, source string) (bool, error)
}

// callbackSource is the session_return_total label for a return the callback caused.
const callbackSource = "callback"

// AllocationEnded serves the Allocator's callback: {"party_id","reason"} for the
// allocation in the path. The caller has already been authenticated with
// session_allocator.key. It answers 204 whether or not the party still exists or is
// still in a match on that allocation, so a repeated or late callback is harmless
// (doc 14 §4.4). A body that is not the contract's is 400, and a store failure is 500;
// the Allocator retries both, and the repair poll catches what it gives up on.
//
// Unknown fields are ignored rather than refused, unlike the player routes: a field the
// Allocator adds later must not turn every callback into a retry.
func (h *Handlers) AllocationEnded(w http.ResponseWriter, r *http.Request) {
	allocationID, err := uuid.Parse(r.PathValue("allocation_id"))
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_request", "allocation_id must be a UUID")
		return
	}
	var req struct {
		PartyID string `json:"party_id"`
		Reason  string `json:"reason"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCallbackBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			WriteError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return
		}
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return
	}
	partyID, err := uuid.Parse(req.PartyID)
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_request", "party_id must be a UUID")
		return
	}
	if h.Returns == nil {
		h.internalError(w, r, errors.New("no returner configured for the allocation callback"))
		return
	}
	if _, err := h.Returns.Return(r.Context(), partyID.String(), allocationID.String(), req.Reason, callbackSource); err != nil {
		h.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
