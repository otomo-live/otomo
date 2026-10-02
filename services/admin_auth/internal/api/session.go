// The shared "sign the account in" tail used by password login, MFA verification and
// MFA enrollment's ticket path.
//
// All three end the same way: a fresh refresh family, the refresh cookie, an access
// token and a login.success audit row. Keeping it in one function means the three
// paths cannot drift — an expiry fix or cookie-attribute change lands once.

package api

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"time"

	"uuid"

	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// sessionStore is the subset of *store.DB issueSession needs.
type sessionStore interface {
	RecordLoginSuccess(ctx context.Context, id, ip, ua string, refreshHash []byte, familyID uuid.UUID, refreshTTL time.Duration, actor store.Entry, details map[string]any) error
}

// sessionDeps is the signing side shared by every endpoint that ends in a session.
type sessionDeps struct {
	Store      sessionStore
	Signer     *token.Signer
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Logger     *slog.Logger
}

// issuedSession is the success body's token and identity fields, returned so a caller
// that needs to add fields (confirm's recovery codes) can write one JSON object.
type issuedSession struct {
	AccessToken string
	ExpiresIn   int64
	User        loginUser
}

// authIdentity is the account state the post-password MFA policy needs. Login builds
// it from store.User, onboarding from store.RedeemedUser, so both feed the same switch.
type authIdentity struct {
	ID              string
	Name            string
	Roles           []string
	IsRoot          bool
	TOTPConfirmedAt *time.Time
}

// authPolicyStore is the store surface completeAuthentication needs: the MFA ticket
// mint plus the session record already covered by sessionStore.
type authPolicyStore interface {
	CreateMFATicket(ctx context.Context, userID, purpose string, ttl time.Duration) (string, error)
	sessionStore
}

// completeAuthentication applies the one post-password policy shared by password login
// and onboarding (design/06 §13): the break-glass root is exempt; an account with a
// confirmed factor must verify it; an admin without one must enroll; anyone else gets a
// session. It writes the challenge or the login body and reports exactly one outcome
// label through record, mirroring login's metric contract. The password, status and
// lockout checks are the caller's and must already have passed.
func completeAuthentication(w http.ResponseWriter, r *http.Request, st authPolicyStore, deps sessionDeps, user authIdentity, record func(string)) {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	switch {
	case user.IsRoot:
		// Password-only by design; never challenged and may not enroll.
	case user.TOTPConfirmedAt != nil:
		ticket, err := st.CreateMFATicket(r.Context(), user.ID, store.MFAPurposeVerify, mfaTicketTTL)
		if err != nil {
			record("error")
			internalError(w, r, log, "create mfa verify ticket", err)
			return
		}
		record("mfa_required")
		writeJSON(w, http.StatusOK, mfaChallengeResponse{MFARequired: true, MFATicket: ticket})
		return
	case store.HasRole(user.Roles, "admin"):
		ticket, err := st.CreateMFATicket(r.Context(), user.ID, store.MFAPurposeEnroll, mfaTicketTTL)
		if err != nil {
			record("error")
			internalError(w, r, log, "create mfa enroll ticket", err)
			return
		}
		record("mfa_enrollment_required")
		writeJSON(w, http.StatusOK, mfaChallengeResponse{MFAEnrollmentRequired: true, MFATicket: ticket})
		return
	}

	session, ok := issueSession(w, r, deps, user.ID, user.Name, user.Roles, nil)
	if !ok {
		record("error")
		return
	}
	record("success")
	writeJSON(w, http.StatusOK, loginResponse{
		AccessToken: session.AccessToken,
		ExpiresIn:   session.ExpiresIn,
		User:        session.User,
	})
}

// issueSession mints the refresh token, records the session and audit row, signs the
// access token and sets the cookie. It returns false after writing the COM-5 500 when
// any step fails; the caller must then return immediately. details is written into the
// login.success audit row.
func issueSession(w http.ResponseWriter, r *http.Request, deps sessionDeps, id, name string, roles []string, details map[string]any) (issuedSession, bool) {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	refresh, err := newRefreshToken()
	if err != nil {
		internalError(w, r, log, "mint refresh token", err)
		return issuedSession{}, false
	}
	hash := sha256.Sum256([]byte(refresh))

	actor := store.Entry{ID: id, Name: name}
	if err := deps.Store.RecordLoginSuccess(r.Context(), id, clientIP(r), truncateBytes(r.UserAgent(), maxUserAgent), hash[:], uuid.New(), deps.RefreshTTL, actor, details); err != nil {
		internalError(w, r, log, "record login success", err)
		return issuedSession{}, false
	}

	access, err := deps.Signer.Issue(id, name, roles, time.Now())
	if err != nil {
		internalError(w, r, log, "issue access token", err)
		return issuedSession{}, false
	}

	setRefreshCookie(w, refresh, int(deps.RefreshTTL.Seconds()))
	return issuedSession{
		AccessToken: access,
		ExpiresIn:   int64(deps.AccessTTL.Seconds()),
		User:        loginUser{ID: id, Name: name, Roles: roles},
	}, true
}
