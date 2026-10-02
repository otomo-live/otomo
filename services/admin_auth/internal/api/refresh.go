// POST /admin-auth/refresh — exchanges the refresh cookie for a new access token.
//
// The body and bearer token are ignored: the cookie is the only credential. The
// lookup takes the session row FOR UPDATE, so simultaneous refreshes of one cookie
// serialise and exactly one of them rotates. A presentation inside the configured
// grace window is the benign multi-tab race and succeeds without touching the
// session; outside it, the token is a replay and the whole family is revoked.

package api

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"time"

	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// SessionStore is the subset of *store.DB the session handlers need. It is an
// interface for the same reason LoginStore is: the server package owns the concrete
// wiring, and a handler can be exercised without a database.
type SessionStore interface {
	RefreshSession(ctx context.Context, in store.RefreshInput) (store.RefreshResult, error)
	Logout(ctx context.Context, tokenHash []byte) error
	UserByID(ctx context.Context, id string) (store.User, error)
}

// Store is everything the public handlers need. *store.DB satisfies it.
type Store interface {
	LoginStore
	SessionStore
	AdminStore
	OnboardStore
	AuditStore
	MFAStore
	AccountStore
}

// RefreshDeps is everything the refresh handler needs.
type RefreshDeps struct {
	Store      SessionStore
	Signer     *token.Signer
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Grace      time.Duration
	Logger     *slog.Logger
	// OnResult, when non-nil, is called once per request with the outcome label the
	// server records in its metrics. The api package stays free of Prometheus: the
	// server wraps its counter in this closure.
	OnResult func(result string)
}

// noValidRefreshSession is the one 401 for every unusable refresh cookie — absent,
// unknown, expired, revoked, replayed, or owned by a disabled account. Being
// specific would tell a caller which of those it was, which is an oracle.
const noValidRefreshSession = "no valid refresh session"

// Refresh returns the POST /admin-auth/refresh handler.
func Refresh(deps RefreshDeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	// record is the one place the outcome metric is incremented; every return path
	// calls it exactly once. It is nil-safe so a test that builds RefreshDeps by hand
	// does not have to supply a counter.
	record := func(result string) {
		if deps.OnResult != nil {
			deps.OnResult(result)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(refreshCookieName)
		if err != nil {
			record("invalid")
			clearRefreshCookie(w)
			WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", noValidRefreshSession)
			return
		}

		oldHash := sha256.Sum256([]byte(cookie.Value))
		successor, err := newRefreshToken()
		if err != nil {
			record("error")
			internalError(w, r, log, "mint refresh token", err)
			return
		}
		newHash := sha256.Sum256([]byte(successor))

		now := time.Now()
		result, err := deps.Store.RefreshSession(r.Context(), store.RefreshInput{
			TokenHash:    oldHash[:],
			NewTokenHash: newHash[:],
			RefreshTTL:   deps.RefreshTTL,
			ReuseGrace:   deps.Grace,
			Now:          now,
			IP:           clientIP(r),
			UserAgent:    truncateBytes(r.UserAgent(), maxUserAgent),
		})
		if err != nil {
			record("error")
			internalError(w, r, log, "refresh session", err)
			return
		}

		if result.Outcome != store.RefreshRotated && result.Outcome != store.RefreshGrace {
			if result.Outcome == store.RefreshReused {
				record("reuse_detected")
			} else {
				record("invalid")
			}
			clearRefreshCookie(w)
			WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", noValidRefreshSession)
			return
		}

		access, err := deps.Signer.Issue(result.UserID, result.Name, result.Roles, now)
		if err != nil {
			record("error")
			internalError(w, r, log, "issue access token", err)
			return
		}

		record("success")
		// Only a real rotation replaces the browser's cookie. The grace path must not
		// Set-Cookie: the browser already holds the successor the first tab received,
		// and overwriting it with this request's unused successor would invalidate
		// that first tab's session.
		if result.Outcome == store.RefreshRotated {
			setRefreshCookie(w, successor, int(deps.RefreshTTL.Seconds()))
		}
		writeJSON(w, http.StatusOK, loginResponse{
			AccessToken: access,
			ExpiresIn:   int64(deps.AccessTTL.Seconds()),
			User:        loginUser{ID: result.UserID, Name: result.Name, Roles: result.Roles},
		})
	}
}
