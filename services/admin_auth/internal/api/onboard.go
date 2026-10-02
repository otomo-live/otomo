// The public onboarding endpoints, POST /admin-auth/onboard/lookup and
// POST /admin-auth/onboard.
//
// Both take the link token in the request body rather than the path. The link a person
// receives is …/admin/onboard#token=<t>: the fragment is read by the browser app and
// POSTed here, so the token never appears in a URL that a proxy, a load balancer or a
// Referer header can log.
//
// Lookup tells the page who the link is for and whether it is an invite or a reset.
// Redeem applies the password policy, consumes the link in one transaction and then
// signs the account in with exactly the login path's session, cookie and access token.

package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"uuid"

	"github.com/otomo-live/otomo/services/admin_auth/internal/password"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// invalidLinkMessage is the one body every unusable link gets, whatever made it
// unusable. Distinguishing unknown from used from expired would be an oracle.
const invalidLinkMessage = "this link is invalid or has expired"

// OnboardStore is the subset of *store.DB the onboarding handlers need. It is an
// interface for the same reason LoginStore is: the server package owns the concrete
// wiring, and a handler can be exercised without a database.
type OnboardStore interface {
	LookupLink(ctx context.Context, tokenHash []byte) (store.OnboardLink, error)
	RedeemLink(ctx context.Context, in store.RedeemLinkInput) (store.RedeemedUser, error)
	RecordLoginSuccess(ctx context.Context, id, ip, ua string, refreshHash []byte, familyID uuid.UUID, refreshTTL time.Duration, actor store.Entry, details map[string]any) error
	CreateMFATicket(ctx context.Context, userID, purpose string, ttl time.Duration) (string, error)
}

// OnboardDeps is everything the onboarding handlers need. It mirrors LoginDeps'
// signing side, because a successful redemption signs the account in the same way.
type OnboardDeps struct {
	Store      OnboardStore
	Signer     *token.Signer
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Logger     *slog.Logger
	// OnResult, when non-nil, is called with the same login outcome label the
	// login handler emits for the shared post-password policy.
	OnResult func(result string)
}

// onboardLookupRequest is the lookup body. Unknown fields are rejected by the decoder.
type onboardLookupRequest struct {
	Token string `json:"token"`
}

// onboardLookupResponse tells the page what it is looking at. Role is nil for a
// reset link, which has none.
type onboardLookupResponse struct {
	Purpose   string    `json:"purpose"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      *string   `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
}

// onboardRequest is the redeem body.
type onboardRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// OnboardLookup returns the POST /admin-auth/onboard/lookup handler.
func OnboardLookup(deps OnboardDeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req onboardLookupRequest
		if !decodeAdminBody(w, r, &req) {
			return
		}
		req.Token = strings.TrimSpace(req.Token)
		if req.Token == "" {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "token must not be empty")
			return
		}

		hash := sha256.Sum256([]byte(req.Token))
		link, err := deps.Store.LookupLink(r.Context(), hash[:])
		if errors.Is(err, store.ErrInviteNotFound) {
			WriteError(w, r, http.StatusNotFound, "invalid_link", invalidLinkMessage)
			return
		}
		if err != nil {
			internalError(w, r, log, "look up onboarding link", err)
			return
		}

		writeJSON(w, http.StatusOK, onboardLookupResponse{
			Purpose:   link.Purpose,
			Email:     link.Email,
			Name:      link.Name,
			Role:      link.Role,
			ExpiresAt: link.ExpiresAt,
		})
	}
}

// Onboard returns the POST /admin-auth/onboard handler.
func Onboard(deps OnboardDeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req onboardRequest
		if !decodeAdminBody(w, r, &req) {
			return
		}
		req.Token = strings.TrimSpace(req.Token)
		if req.Token == "" || req.Password == "" {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "token and password must not be empty")
			return
		}

		hash := sha256.Sum256([]byte(req.Token))
		link, err := deps.Store.LookupLink(r.Context(), hash[:])
		if errors.Is(err, store.ErrInviteNotFound) {
			WriteError(w, r, http.StatusNotFound, "invalid_link", invalidLinkMessage)
			return
		}
		if err != nil {
			internalError(w, r, log, "look up onboarding link", err)
			return
		}

		// The policy needs the link's address, so it runs after the lookup. A bad
		// password never consumes the link: the redemption below is what stamps it.
		if err := password.Validate(req.Password, link.Email); err != nil {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		phc, err := password.Hash(req.Password)
		if err != nil {
			internalError(w, r, log, "hash onboarding password", err)
			return
		}

		user, err := deps.Store.RedeemLink(r.Context(), store.RedeemLinkInput{
			TokenHash:    hash[:],
			PasswordHash: phc,
		})
		switch {
		case errors.Is(err, store.ErrEmailExists):
			WriteError(w, r, http.StatusConflict, "already_exists", "a staff user already has that email")
			return
		case errors.Is(err, store.ErrInviteNotFound):
			WriteError(w, r, http.StatusNotFound, "invalid_link", invalidLinkMessage)
			return
		case err != nil:
			internalError(w, r, log, "redeem onboarding link", err)
			return
		}

		// From here on this is the login success path, including the MFA policy:
		// root gets a session, a confirmed factor gets a verify challenge and an
		// admin without one gets an enroll challenge, with no token or cookie.
		// The link is consumed and the password set in every case; the MFA step
		// then continues on /admin-auth/mfa/verify or /enroll + /confirm.
		completeAuthentication(w, r, deps.Store, sessionDeps{
			Store:      deps.Store,
			Signer:     deps.Signer,
			AccessTTL:  deps.AccessTTL,
			RefreshTTL: deps.RefreshTTL,
			Logger:     log,
		}, authIdentity{
			ID:              user.ID,
			Name:            user.Name,
			Roles:           user.Roles,
			IsRoot:          user.IsRoot,
			TOTPConfirmedAt: user.TOTPConfirmedAt,
		}, func(result string) {
			if deps.OnResult != nil {
				deps.OnResult(result)
			}
		})
	}
}
