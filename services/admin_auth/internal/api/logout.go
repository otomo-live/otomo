// POST /admin-auth/logout — revokes the refresh cookie's whole rotation family and
// clears the cookie. It is idempotent and always answers 204, even when no cookie was
// sent or the cookie names no session: a logout that reported "nothing to do" would
// only give a client something to branch on.

package api

import (
	"crypto/sha256"
	"log/slog"
	"net/http"
)

// LogoutDeps is everything the logout handler needs.
type LogoutDeps struct {
	Store  SessionStore
	Logger *slog.Logger
}

// Logout returns the POST /admin-auth/logout handler.
func Logout(deps LogoutDeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(refreshCookieName); err == nil {
			hash := sha256.Sum256([]byte(cookie.Value))
			if err := deps.Store.Logout(r.Context(), hash[:]); err != nil {
				internalError(w, r, log, "logout", err)
				return
			}
		}

		clearRefreshCookie(w)
		w.WriteHeader(http.StatusNoContent)
	}
}
