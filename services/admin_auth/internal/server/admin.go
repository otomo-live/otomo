// The in-service role gate for the staff business routes.
//
// gateway_dev already enforces roles per route, but that is one process's opinion and
// a future route or a direct east-west call must not be able to skip it. This
// middleware repeats the check from the database: verify the bearer the way /me does,
// load the caller by sub, require an active account whose row carries a role at least
// as strong as the route's minimum, and only then put the caller on the context for
// the handler to audit.

package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/otomo-live/otomo/services/admin_auth/internal/api"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// roleRank orders the three staff roles from weakest to strongest. A route's minimum
// is satisfied by any role at or above it.
var roleRank = map[string]int{
	"viewer":   1,
	"live_ops": 2,
	"admin":    3,
}

// roleAtLeast reports whether roles carries at least the minimum. A role unknown to
// the DB constraint does not count toward the minimum.
func roleAtLeast(roles []string, min string) bool {
	need, ok := roleRank[min]
	if !ok {
		return false
	}
	for _, r := range roles {
		if rank, ok := roleRank[r]; ok && rank >= need {
			return true
		}
	}
	return false
}

// requireRole wraps a route with bearer verification, a database lookup and a minimum
// role check.
//
// The refusals mirror Gateway's codes exactly so the admin UI cannot tell which edge
// answered: a bad or absent token is the verifier's 401 reason, a disabled or deleted
// account is invalid_token, and an active account below the minimum is 403
// insufficient_role.
func (s *Server) requireRole(min string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, reason := s.deps.Verifier.Verify(api.BearerToken(r))
			if reason != "" {
				api.WriteError(w, r, http.StatusUnauthorized, reason, token.MessageFor(reason))
				return
			}

			// Roles come from the row, never from the token: a token can outlive the
			// role it was minted with, and a disabled account must stop resolving
			// even while its token is still cryptographically valid.
			user, err := s.deps.Store.UserByID(r.Context(), claims.Subject)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					api.WriteError(w, r, http.StatusUnauthorized, token.ReasonInvalidToken, token.MessageFor(token.ReasonInvalidToken))
					return
				}
				s.log.LogAttrs(r.Context(), slog.LevelError, "internal error",
					slog.String("operation", "load staff caller"),
					slog.Any("error", err),
					slog.String("request_id", api.RequestID(r.Context())),
				)
				api.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
				return
			}
			if user.Status != "active" {
				api.WriteError(w, r, http.StatusUnauthorized, token.ReasonInvalidToken, token.MessageFor(token.ReasonInvalidToken))
				return
			}
			if !roleAtLeast(user.Roles, min) {
				api.WriteError(w, r, http.StatusForbidden, "insufficient_role", fmt.Sprintf("the %s role is required", min))
				return
			}

			ctx := api.WithAdmin(r.Context(), api.Admin{ID: user.ID, Name: user.Name, IsRoot: user.IsRoot})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// requireAdmin is requireRole with the admin minimum, kept as a named helper so the
// user-management routes read the same as they did when the check lived here.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireRole("admin")(next)
}
