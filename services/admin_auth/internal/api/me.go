// GET /admin-auth/me — answers "is this token still good, and who is it for".
//
// The token is verified in-process against the same active keys this service
// publishes in its JWKS, then the user is loaded by `sub` from the database. Name and
// roles come from that row, never from the token: a token can outlive the roles it
// was minted with, and a disabled account must stop resolving even while an
// outstanding access token is still cryptographically valid.

package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// meResponse is the login user plus two account facts the account page needs
// and nothing else reports: whether a second factor is confirmed, and whether this is
// the password-only break-glass root. Both come from the row, like name and roles.
type meResponse struct {
	loginUser
	IsRoot     bool `json:"is_root"`
	MFAEnabled bool `json:"mfa_enabled"`
}

// MeDeps is everything the /me handler needs.
type MeDeps struct {
	Store    SessionStore
	Verifier *token.Verifier
	Logger   *slog.Logger
}

// Me returns the GET /admin-auth/me handler.
func Me(deps MeDeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		claims, reason := deps.Verifier.Verify(BearerToken(r))
		if reason != "" {
			WriteError(w, r, http.StatusUnauthorized, reason, token.MessageFor(reason))
			return
		}

		user, err := deps.Store.UserByID(r.Context(), claims.Subject)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				WriteError(w, r, http.StatusUnauthorized, token.ReasonInvalidToken, token.MessageFor(token.ReasonInvalidToken))
				return
			}
			internalError(w, r, log, "load staff user", err)
			return
		}
		if user.Status != "active" {
			WriteError(w, r, http.StatusUnauthorized, token.ReasonInvalidToken, token.MessageFor(token.ReasonInvalidToken))
			return
		}

		writeJSON(w, http.StatusOK, meResponse{
			loginUser:  loginUser{ID: user.ID, Name: user.Name, Roles: user.Roles},
			IsRoot:     user.IsRoot,
			MFAEnabled: user.TOTPConfirmedAt != nil,
		})
	}
}

// BearerToken returns the credential from the Authorization header, or "" when the
// header is absent or is not a bearer credential. It mirrors Gateway's parser so a
// header Gateway would accept is not rejected here for a formatting reason.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}
