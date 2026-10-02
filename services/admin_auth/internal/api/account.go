// The self-service account endpoints under /admin-auth/account.
//
// Every route is authenticated exactly like GET /admin-auth/me: the bearer access
// token is verified in-process, then the subject is re-read from the database and an
// account that is disabled or gone is refused with the same 401. A handler never
// trusts a role or a status from the token. The refresh cookie is read only to
// identify the caller's own session when a request has to distinguish it from the
// others.

package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/otomo-live/otomo/services/admin_auth/internal/mfa"
	"github.com/otomo-live/otomo/services/admin_auth/internal/password"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// accountRecoveryCodeCount is how many codes a regeneration issues. Exactly ten is
// the contract, matching enrollment.
const accountRecoveryCodeCount = 10

// AccountStore is the subset of *store.DB the account handlers need.
type AccountStore interface {
	UserByID(ctx context.Context, id string) (store.User, error)
	MFAUserByID(ctx context.Context, id string) (store.MFAUser, error)
	RecordLoginFailure(ctx context.Context, id string, maxFailures int, lockFor time.Duration) (bool, error)
	ChangePassword(ctx context.Context, userID, newHash string, keepTokenHash []byte, actor store.Entry) (int, error)
	ListAccountSessions(ctx context.Context, userID string, currentTokenHash []byte) ([]store.AccountSession, error)
	RevokeOtherSessions(ctx context.Context, userID string, currentTokenHash []byte, actor store.Entry) (store.RevokeSessionsResult, error)
	ReplaceRecoveryCodes(ctx context.Context, userID string, step int64, hashes [][]byte, actor store.Entry) error
	DisableMFA(ctx context.Context, userID string, step *int64, recoveryHash []byte, actor store.Entry) error
}

// AccountDeps is everything the account handlers need. Verify and Now are injection
// points for tests; production leaves them nil and gets password.Verify and time.Now.
type AccountDeps struct {
	Store       AccountStore
	Verifier    *token.Verifier
	Key         []byte
	MaxFailures int
	Lockout     time.Duration
	Logger      *slog.Logger
	Verify      func(phc, pw string) (bool, error)
	Now         func() time.Time
}

type passwordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type accountSessionJSON struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	IP         *string   `json:"ip"`
	UserAgent  *string   `json:"user_agent"`
	Current    bool      `json:"current"`
}

type accountSessionsResponse struct {
	Sessions []accountSessionJSON `json:"sessions"`
}

type revokeOthersResponse struct {
	Revoked     int  `json:"revoked"`
	CurrentKept bool `json:"current_kept"`
}

type accountMFACodeRequest struct {
	Code string `json:"code"`
}

type recoveryCodesResponse struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

func accountLogger(deps AccountDeps) *slog.Logger {
	if deps.Logger != nil {
		return deps.Logger
	}
	return slog.Default()
}

// accountVerify is the argon2 comparison the password change runs, defaulting to the
// production implementation.
func accountVerify(deps AccountDeps) func(phc, pw string) (bool, error) {
	if deps.Verify != nil {
		return deps.Verify
	}
	return password.Verify
}

// accountNow is the clock the TOTP window is judged against.
func accountNow(deps AccountDeps) func() time.Time {
	if deps.Now != nil {
		return deps.Now
	}
	return time.Now
}

// currentRefreshHash returns the sha256 of the refresh cookie, or nil when the
// request carried none. The __Host- cookie has Path=/, so it is present on these
// routes whenever the browser has one.
func currentRefreshHash(r *http.Request) []byte {
	cookie, err := r.Cookie(refreshCookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(cookie.Value))
	return sum[:]
}

// bearerSubject verifies the bearer token and returns its subject. It writes the
// verifier's 401 and returns false when the token is absent or unusable.
func (deps AccountDeps) bearerSubject(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, reason := deps.Verifier.Verify(BearerToken(r))
	if reason != "" {
		WriteError(w, r, http.StatusUnauthorized, reason, token.MessageFor(reason))
		return "", false
	}
	return claims.Subject, true
}

// activeUser verifies the bearer and loads the account, requiring an active status.
// It mirrors /me: a missing or disabled account is the same 401 as a bad token.
func (deps AccountDeps) activeUser(w http.ResponseWriter, r *http.Request, log *slog.Logger) (store.User, bool) {
	subject, ok := deps.bearerSubject(w, r)
	if !ok {
		return store.User{}, false
	}
	user, err := deps.Store.UserByID(r.Context(), subject)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, http.StatusUnauthorized, token.ReasonInvalidToken, token.MessageFor(token.ReasonInvalidToken))
		return store.User{}, false
	}
	if err != nil {
		internalError(w, r, log, "load account user", err)
		return store.User{}, false
	}
	if user.Status != "active" {
		WriteError(w, r, http.StatusUnauthorized, token.ReasonInvalidToken, token.MessageFor(token.ReasonInvalidToken))
		return store.User{}, false
	}
	return user, true
}

// activeMFAUser verifies the bearer and loads the account with its TOTP material,
// requiring an active status. The separate read is what lets the MFA handlers open
// the sealed secret without every other account read selecting it.
func (deps AccountDeps) activeMFAUser(w http.ResponseWriter, r *http.Request, log *slog.Logger) (store.MFAUser, bool) {
	subject, ok := deps.bearerSubject(w, r)
	if !ok {
		return store.MFAUser{}, false
	}
	user, err := deps.Store.MFAUserByID(r.Context(), subject)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, http.StatusUnauthorized, token.ReasonInvalidToken, token.MessageFor(token.ReasonInvalidToken))
		return store.MFAUser{}, false
	}
	if err != nil {
		internalError(w, r, log, "load account user", err)
		return store.MFAUser{}, false
	}
	if user.Status != "active" {
		WriteError(w, r, http.StatusUnauthorized, token.ReasonInvalidToken, token.MessageFor(token.ReasonInvalidToken))
		return store.MFAUser{}, false
	}
	return user, true
}

// AccountPassword returns POST /admin-auth/account/password.
func AccountPassword(deps AccountDeps) http.HandlerFunc {
	log := accountLogger(deps)
	verify := accountVerify(deps)

	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := deps.activeUser(w, r, log)
		if !ok {
			return
		}

		var req passwordChangeRequest
		if !decodeAdminBody(w, r, &req) {
			return
		}
		if req.CurrentPassword == "" || req.NewPassword == "" {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "current_password and new_password must not be empty")
			return
		}

		// Verify before the lock check, exactly as login does: a locked account still
		// costs the same argon2 work, and the failure is the same 401.
		matches, err := verify(user.PasswordHash, req.CurrentPassword)
		if err != nil {
			internalError(w, r, log, "verify current password", err)
			return
		}
		now := time.Now()
		if user.LockedUntil != nil && user.LockedUntil.After(now) {
			WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", invalidCredentialsMessage)
			return
		}
		if !matches {
			if _, err := deps.Store.RecordLoginFailure(r.Context(), user.ID, deps.MaxFailures, deps.Lockout); err != nil {
				internalError(w, r, log, "record password failure", err)
				return
			}
			WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", invalidCredentialsMessage)
			return
		}

		if err := password.Validate(req.NewPassword, user.Email); err != nil {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		if req.NewPassword == req.CurrentPassword {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "new_password must differ from the current password")
			return
		}

		hash, err := password.Hash(req.NewPassword)
		if err != nil {
			internalError(w, r, log, "hash new password", err)
			return
		}
		actor := store.Entry{ID: user.ID, Name: user.Name}
		if _, err := deps.Store.ChangePassword(r.Context(), user.ID, hash, currentRefreshHash(r), actor); err != nil {
			internalError(w, r, log, "change password", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// AccountSessions returns GET /admin-auth/account/sessions.
func AccountSessions(deps AccountDeps) http.HandlerFunc {
	log := accountLogger(deps)

	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := deps.activeUser(w, r, log)
		if !ok {
			return
		}

		sessions, err := deps.Store.ListAccountSessions(r.Context(), user.ID, currentRefreshHash(r))
		if err != nil {
			internalError(w, r, log, "list account sessions", err)
			return
		}
		out := make([]accountSessionJSON, 0, len(sessions))
		for _, s := range sessions {
			out = append(out, accountSessionJSON{
				ID:         s.ID,
				CreatedAt:  s.CreatedAt,
				LastUsedAt: s.LastUsedAt,
				IP:         s.IP,
				UserAgent:  s.UserAgent,
				Current:    s.Current,
			})
		}
		writeJSON(w, http.StatusOK, accountSessionsResponse{Sessions: out})
	}
}

// AccountRevokeOtherSessions returns POST /admin-auth/account/sessions/revoke-others.
func AccountRevokeOtherSessions(deps AccountDeps) http.HandlerFunc {
	log := accountLogger(deps)

	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := deps.activeUser(w, r, log)
		if !ok {
			return
		}

		actor := store.Entry{ID: user.ID, Name: user.Name}
		result, err := deps.Store.RevokeOtherSessions(r.Context(), user.ID, currentRefreshHash(r), actor)
		if err != nil {
			internalError(w, r, log, "revoke other sessions", err)
			return
		}
		writeJSON(w, http.StatusOK, revokeOthersResponse{Revoked: result.Revoked, CurrentKept: result.CurrentKept})
	}
}

// AccountRegenerateRecoveryCodes returns POST /admin-auth/account/mfa/recovery-codes.
func AccountRegenerateRecoveryCodes(deps AccountDeps) http.HandlerFunc {
	log := accountLogger(deps)
	now := accountNow(deps)

	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := deps.activeMFAUser(w, r, log)
		if !ok {
			return
		}

		var req accountMFACodeRequest
		if !decodeAdminBody(w, r, &req) {
			return
		}
		req.Code = strings.TrimSpace(req.Code)
		if req.Code == "" {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "code must not be empty")
			return
		}
		if user.TOTPConfirmedAt == nil {
			WriteError(w, r, http.StatusConflict, "mfa_not_enrolled", "this account has no confirmed second factor")
			return
		}

		secret, err := mfa.Open(deps.Key, user.ID, user.TOTPSecretEnc)
		if err != nil {
			internalError(w, r, log, "open mfa secret", err)
			return
		}
		step, ok := mfa.Validate(secret, req.Code, now())
		if !ok {
			WriteError(w, r, http.StatusUnauthorized, "invalid_code", invalidCodeMessage)
			return
		}

		codes, err := mfa.RecoveryCodes(accountRecoveryCodeCount)
		if err != nil {
			internalError(w, r, log, "generate recovery codes", err)
			return
		}
		hashes := make([][]byte, 0, len(codes))
		for _, code := range codes {
			hashes = append(hashes, mfa.HashRecoveryCode(code))
		}

		actor := store.Entry{ID: user.ID, Name: user.Name}
		if err := deps.Store.ReplaceRecoveryCodes(r.Context(), user.ID, step, hashes, actor); err != nil {
			switch {
			case errors.Is(err, store.ErrTOTPReplay):
				WriteError(w, r, http.StatusUnauthorized, "invalid_code", invalidCodeMessage)
			case errors.Is(err, store.ErrMFANotEnrolled):
				WriteError(w, r, http.StatusConflict, "mfa_not_enrolled", "this account has no confirmed second factor")
			default:
				internalError(w, r, log, "replace recovery codes", err)
			}
			return
		}
		writeJSON(w, http.StatusOK, recoveryCodesResponse{RecoveryCodes: codes})
	}
}

// AccountDisableMFA returns POST /admin-auth/account/mfa/disable. Decision D5: only
// an account without the admin role may turn its second factor off, and root may not
// use the route at all.
func AccountDisableMFA(deps AccountDeps) http.HandlerFunc {
	log := accountLogger(deps)
	now := accountNow(deps)

	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := deps.activeMFAUser(w, r, log)
		if !ok {
			return
		}

		var req accountMFACodeRequest
		if !decodeAdminBody(w, r, &req) {
			return
		}
		req.Code = strings.TrimSpace(req.Code)
		if req.Code == "" {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "code must not be empty")
			return
		}
		if user.IsRoot {
			WriteError(w, r, http.StatusForbidden, "root_protected", "the break-glass root account is protected")
			return
		}
		if store.HasRole(user.Roles, "admin") {
			WriteError(w, r, http.StatusForbidden, "mfa_required", "MFA is required for admins")
			return
		}
		if user.TOTPConfirmedAt == nil {
			WriteError(w, r, http.StatusConflict, "mfa_not_enrolled", "this account has no confirmed second factor")
			return
		}

		var (
			step         *int64
			recoveryHash []byte
		)
		if secret, err := mfa.Open(deps.Key, user.ID, user.TOTPSecretEnc); err == nil {
			if matched, valid := mfa.Validate(secret, req.Code, now()); valid {
				step = &matched
			}
		}
		if step == nil {
			if normalized, valid := mfa.NormalizeRecoveryCode(req.Code); valid {
				recoveryHash = mfa.HashRecoveryCode(normalized)
			}
		}
		if step == nil && recoveryHash == nil {
			WriteError(w, r, http.StatusUnauthorized, "invalid_code", invalidCodeMessage)
			return
		}

		actor := store.Entry{ID: user.ID, Name: user.Name}
		err := deps.Store.DisableMFA(r.Context(), user.ID, step, recoveryHash, actor)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, store.ErrRootProtected):
			WriteError(w, r, http.StatusForbidden, "root_protected", "the break-glass root account is protected")
		case errors.Is(err, store.ErrMFARequired):
			WriteError(w, r, http.StatusForbidden, "mfa_required", "MFA is required for admins")
		case errors.Is(err, store.ErrMFANotEnrolled):
			WriteError(w, r, http.StatusConflict, "mfa_not_enrolled", "this account has no confirmed second factor")
		case errors.Is(err, store.ErrTOTPReplay), errors.Is(err, store.ErrRecoveryCodeInvalid):
			WriteError(w, r, http.StatusUnauthorized, "invalid_code", invalidCodeMessage)
		default:
			internalError(w, r, log, "disable mfa", err)
		}
	}
}
