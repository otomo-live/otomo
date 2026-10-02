// Profile routes (SE-2): POST /me/init, GET /me and PATCH /me.
//
// The wire shape is the one design/12-godot-sdk-guide.md §7.1 gives and the SDK parses
// (godot-plugin/addons/otomo/Core/Session/ISessionClient.cs): player_id, display_name,
// and discriminator as a number.

package api

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/rules"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

// maxProfileBody caps a PATCH /me body. It carries one short name.
const maxProfileBody = 1 << 10

// provisionalPrefix starts every name POST /me/init assigns. Four random digits follow,
// so the provisional names are spread over many (name, discriminator) pairs and a busy
// server does not run one name out of discriminators.
const provisionalPrefix = "Player"

// profileResponse is the profile on the wire.
type profileResponse struct {
	PlayerID      string `json:"player_id"`
	DisplayName   string `json:"display_name"`
	Discriminator int    `json:"discriminator"`
}

func profileFrom(p store.Profile) profileResponse {
	return profileResponse{
		PlayerID:      p.PlayerID.String(),
		DisplayName:   p.DisplayName,
		Discriminator: p.Discriminator,
	}
}

// renameRequest is the PATCH /me body.
type renameRequest struct {
	DisplayName *string `json:"display_name"`
}

// provisionalName returns a name such as "Player4417". It is assigned by the server,
// so it is not checked against the name rules; the player replaces it with PATCH /me,
// and that first rename does not wait for the cooldown.
func provisionalName() string {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		// crypto/rand does not fail on supported platforms. A fixed suffix still works:
		// the discriminator is random and collisions are retried.
		return provisionalPrefix
	}
	return fmt.Sprintf("%s%04d", provisionalPrefix, n.Int64())
}

// initProfile serves POST /me/init: the player's profile, created on the first call.
// It answers 200 either way, so a client can call it after every login.
func (h *Handlers) initProfile(w http.ResponseWriter, r *http.Request) {
	if h.Profiles == nil {
		h.internalError(w, r, errors.New("no profile store configured"))
		return
	}
	id := auth.IdentityFrom(r.Context()).PlayerUUID()

	p, _, err := h.Profiles.InitProfile(r.Context(), id, provisionalName)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, profileFrom(p))
}

// getProfile serves GET /me.
func (h *Handlers) getProfile(w http.ResponseWriter, r *http.Request) {
	if h.Profiles == nil {
		h.internalError(w, r, errors.New("no profile store configured"))
		return
	}
	id := auth.IdentityFrom(r.Context()).PlayerUUID()

	p, err := h.Profiles.GetProfile(r.Context(), id)
	if errors.Is(err, store.ErrProfileNotFound) {
		writeProfileNotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, profileFrom(p))
}

// renameProfile serves PATCH /me {"display_name"}. The name rules and the cooldown come
// from the rules in force for this request.
func (h *Handlers) renameProfile(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	if !decodeBody(w, r, maxProfileBody, &req) {
		return
	}
	if req.DisplayName == nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_display_name", "display_name is required")
		return
	}

	names := h.rules().Names
	name, err := names.Check(*req.DisplayName)
	if err != nil {
		var nameErr *rules.NameError
		if errors.As(err, &nameErr) {
			WriteError(w, r, http.StatusBadRequest, "invalid_display_name", nameErr.Reason)
			return
		}
		h.internalError(w, r, err)
		return
	}

	if h.Profiles == nil {
		h.internalError(w, r, errors.New("no profile store configured"))
		return
	}
	id := auth.IdentityFrom(r.Context()).PlayerUUID()

	p, err := h.Profiles.RenameProfile(r.Context(), id, name, names.RenameCooldown())
	var tooSoon *store.RenameTooSoonError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, profileFrom(p))
	case errors.As(err, &tooSoon):
		// Retry-After in whole seconds, rounded up so a client that waits exactly that
		// long is not refused again.
		wait := time.Until(tooSoon.RetryAt)
		secs := int64((wait + time.Second - 1) / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
		WriteError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded",
			fmt.Sprintf("display name may change once every %d hours", names.RenameCooldownHours))
	case errors.Is(err, store.ErrProfileNotFound):
		writeProfileNotFound(w, r)
	case errors.Is(err, store.ErrNameUnavailable):
		WriteError(w, r, http.StatusConflict, "name_unavailable", "too many players have that display name; choose another")
	default:
		h.internalError(w, r, err)
	}
}

// writeProfileNotFound is the answer for a player who has not called POST /me/init yet.
func writeProfileNotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotFound, "profile_not_found", "no profile yet; call POST /me/init first")
}
