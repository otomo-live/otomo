// The MFA endpoints: POST /admin-auth/mfa/verify, /enroll and /confirm.
//
// verify finishes a password login that answered mfa_required. enroll and confirm set
// up the factor; they accept either the short-lived enroll ticket minted by login or,
// for an already signed-in account on its account page, an ordinary bearer access
// token. The same strict body decoder the onboarding and user-management routes use is
// applied here, so the MFA surface cannot accept a shape its siblings reject.

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
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

const (
	// mfaTicketTTL bounds a login challenge. It only has to outlast the walk from
	// the password form to the code prompt.
	mfaTicketTTL = 5 * time.Minute

	// mfaRecoveryCodeCount is how many one-time codes confirm issues. Exactly ten
	// is the contract.
	mfaRecoveryCodeCount = 10

	// invalidTicketMessage is the one 401 for every unusable ticket — unknown,
	// expired, used, or belonging to an account that was disabled meanwhile.
	invalidTicketMessage = "this sign-in attempt has expired; sign in again"

	// invalidCodeMessage is the one 401 for a wrong TOTP or recovery code.
	invalidCodeMessage = "the code is not valid"
)

// MFAStore is the subset of *store.DB the MFA handlers need.
type MFAStore interface {
	CreateMFATicket(ctx context.Context, userID, purpose string, ttl time.Duration) (string, error)
	MFATicketByHash(ctx context.Context, tokenHash []byte) (store.MFATicket, error)
	MFAUserByID(ctx context.Context, id string) (store.MFAUser, error)
	MarkTOTPStep(ctx context.Context, userID string, step int64) (bool, error)
	ConsumeRecoveryCode(ctx context.Context, userID string, codeHash []byte) (int, error)
	MarkMFATicketUsed(ctx context.Context, id string) error
	ClaimMFAAttempt(ctx context.Context, tokenHash []byte, purpose string, maxAttempts int) (store.MFATicket, error)
	EnrollTOTP(ctx context.Context, userID string, sealed []byte) error
	ConfirmTOTP(ctx context.Context, userID string, step int64, recoveryHashes [][]byte) error
	sessionStore
}

// MFADeps is everything the MFA handlers need. Key is the AES-256 key secrets are
// sealed under; Verifier checks the bearer form of the enroll/confirm auth.
type MFADeps struct {
	Store      MFAStore
	Signer     *token.Signer
	Verifier   *token.Verifier
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Key        []byte
	Logger     *slog.Logger
	// OnResult, when non-nil, is called once per request with the staff_mfa_total
	// outcome label.
	OnResult func(result string)
	// Now is the clock the TOTP window is evaluated against. Production leaves it
	// nil (time.Now); a test injects a fixed one.
	Now func() time.Time
}

// mfaChallengeResponse is login's 200 when a second factor is required. Exactly one of
// the two boolean fields is set.
type mfaChallengeResponse struct {
	MFARequired           bool   `json:"mfa_required,omitempty"`
	MFAEnrollmentRequired bool   `json:"mfa_enrollment_required,omitempty"`
	MFATicket             string `json:"mfa_ticket"`
}

type mfaVerifyRequest struct {
	MFATicket string `json:"mfa_ticket"`
	Code      string `json:"code"`
}

type mfaEnrollRequest struct {
	MFATicket string `json:"mfa_ticket"`
}

type mfaEnrollResponse struct {
	Secret     string `json:"secret"`
	OTPAuthURL string `json:"otpauth_url"`
}

type mfaConfirmRequest struct {
	MFATicket string `json:"mfa_ticket"`
	Code      string `json:"code"`
}

// mfaConfirmResponse is always the recovery codes; with a ticket it also carries the
// login success fields, so enrollment ends signed in.
type mfaConfirmResponse struct {
	RecoveryCodes []string   `json:"recovery_codes"`
	AccessToken   string     `json:"access_token,omitempty"`
	ExpiresIn     int64      `json:"expires_in,omitempty"`
	User          *loginUser `json:"user,omitempty"`
}

// MFAVerify returns the POST /admin-auth/mfa/verify handler.
func MFAVerify(deps MFADeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	record := func(result string) {
		if deps.OnResult != nil {
			deps.OnResult(result)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req mfaVerifyRequest
		if !decodeAdminBody(w, r, &req) {
			record("invalid_code")
			return
		}
		req.MFATicket = strings.TrimSpace(req.MFATicket)
		req.Code = strings.TrimSpace(req.Code)
		if req.MFATicket == "" {
			record("invalid_ticket")
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "mfa_ticket must not be empty")
			return
		}
		if req.Code == "" {
			record("invalid_code")
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "code must not be empty")
			return
		}

		hash := sha256.Sum256([]byte(req.MFATicket))
		// Spend an attempt before any code is checked, atomically, so concurrent
		// requests cannot get more than MFAMaxAttempts guesses at one ticket.
		ticket, err := deps.Store.ClaimMFAAttempt(r.Context(), hash[:], store.MFAPurposeVerify, store.MFAMaxAttempts)
		if errors.Is(err, store.ErrTicketInvalid) {
			record("invalid_ticket")
			WriteError(w, r, http.StatusUnauthorized, "invalid_ticket", invalidTicketMessage)
			return
		}
		if err != nil {
			record("error")
			internalError(w, r, log, "claim mfa attempt", err)
			return
		}

		user, err := deps.Store.MFAUserByID(r.Context(), ticket.UserID)
		if errors.Is(err, store.ErrNotFound) {
			record("invalid_ticket")
			WriteError(w, r, http.StatusUnauthorized, "invalid_ticket", invalidTicketMessage)
			return
		}
		if err != nil {
			record("error")
			internalError(w, r, log, "load mfa user", err)
			return
		}
		// The account can be disabled or locked between the password step and the
		// code step; that must invalidate the whole attempt, not just the code.
		if user.Status != "active" || user.TOTPConfirmedAt == nil ||
			(user.LockedUntil != nil && user.LockedUntil.After(now())) {
			record("invalid_ticket")
			WriteError(w, r, http.StatusUnauthorized, "invalid_ticket", invalidTicketMessage)
			return
		}

		// A TOTP is tried first; a recovery code only when it does not normalize to
		// a TOTP-shaped value. A valid TOTP whose step was already used is a replay
		// and falls through to the wrong-code path.
		if secret, err := mfa.Open(deps.Key, user.ID, user.TOTPSecretEnc); err == nil {
			if step, ok := mfa.Validate(secret, req.Code, now()); ok {
				moved, err := deps.Store.MarkTOTPStep(r.Context(), user.ID, step)
				if err != nil {
					record("error")
					internalError(w, r, log, "record totp step", err)
					return
				}
				if moved {
					deps.finishVerify(w, r, user, ticket.ID, "totp", log, record)
					return
				}
			}
		}

		if norm, ok := mfa.NormalizeRecoveryCode(req.Code); ok {
			if _, err := deps.Store.ConsumeRecoveryCode(r.Context(), user.ID, mfa.HashRecoveryCode(norm)); err == nil {
				deps.finishVerify(w, r, user, ticket.ID, "recovery_code", log, record)
				return
			} else if !errors.Is(err, store.ErrRecoveryCodeInvalid) {
				record("error")
				internalError(w, r, log, "consume recovery code", err)
				return
			}
		}

		// The attempt was already counted when the ticket was claimed.
		record("invalid_code")
		WriteError(w, r, http.StatusUnauthorized, "invalid_code", invalidCodeMessage)
	}
}

// finishVerify consumes the ticket and issues the session, then writes login's success
// body. It returns after writing either that body or a 500.
func (deps MFADeps) finishVerify(w http.ResponseWriter, r *http.Request, user store.MFAUser, ticketID, method string, log *slog.Logger, record func(string)) {
	if err := deps.Store.MarkMFATicketUsed(r.Context(), ticketID); err != nil {
		if errors.Is(err, store.ErrTicketInvalid) {
			// A concurrent request with this ticket signed in first.
			record("invalid_ticket")
			WriteError(w, r, http.StatusUnauthorized, "invalid_ticket", invalidTicketMessage)
			return
		}
		record("error")
		internalError(w, r, log, "consume mfa ticket", err)
		return
	}

	session, ok := issueSession(w, r, deps.session(), user.ID, user.Name, user.Roles, map[string]any{"mfa": method})
	if !ok {
		record("error")
		return
	}

	if method == "recovery_code" {
		record("recovery_code")
	} else {
		record("verified")
	}
	writeJSON(w, http.StatusOK, loginResponse{
		AccessToken: session.AccessToken,
		ExpiresIn:   session.ExpiresIn,
		User:        session.User,
	})
}

// MFAEnroll returns the POST /admin-auth/mfa/enroll handler.
func MFAEnroll(deps MFADeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	record := func(result string) {
		if deps.OnResult != nil {
			deps.OnResult(result)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req mfaEnrollRequest
		if !decodeAdminBody(w, r, &req) {
			record("invalid_ticket")
			return
		}

		user, _, ok := deps.resolveEnrollActor(w, r, req.MFATicket, log, record)
		if !ok {
			return
		}
		if user.IsRoot {
			WriteError(w, r, http.StatusForbidden, "mfa_not_allowed", "the break-glass root account cannot use MFA")
			return
		}

		secret, err := mfa.GenerateSecret()
		if err != nil {
			record("error")
			internalError(w, r, log, "generate mfa secret", err)
			return
		}
		sealed, err := mfa.Seal(deps.Key, user.ID, secret)
		if err != nil {
			record("error")
			internalError(w, r, log, "seal mfa secret", err)
			return
		}
		if err := deps.Store.EnrollTOTP(r.Context(), user.ID, sealed); err != nil {
			if errors.Is(err, store.ErrMFAAlreadyEnabled) {
				WriteError(w, r, http.StatusConflict, "mfa_already_enabled", "this account already has a confirmed second factor")
				return
			}
			record("error")
			internalError(w, r, log, "store mfa secret", err)
			return
		}

		writeJSON(w, http.StatusOK, mfaEnrollResponse{
			Secret:     mfa.EncodeSecret(secret),
			OTPAuthURL: mfa.OTPAuthURL(user.Email, secret),
		})
	}
}

// MFAConfirm returns the POST /admin-auth/mfa/confirm handler.
func MFAConfirm(deps MFADeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	record := func(result string) {
		if deps.OnResult != nil {
			deps.OnResult(result)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req mfaConfirmRequest
		if !decodeAdminBody(w, r, &req) {
			record("invalid_code")
			return
		}
		req.Code = strings.TrimSpace(req.Code)

		user, ticket, ok := deps.resolveEnrollActor(w, r, req.MFATicket, log, record)
		if !ok {
			return
		}
		if user.IsRoot {
			WriteError(w, r, http.StatusForbidden, "mfa_not_allowed", "the break-glass root account cannot use MFA")
			return
		}
		if user.TOTPConfirmedAt != nil {
			WriteError(w, r, http.StatusConflict, "mfa_already_enabled", "this account already has a confirmed second factor")
			return
		}
		if user.TOTPSecretEnc == nil {
			WriteError(w, r, http.StatusConflict, "mfa_not_enrolling", "start MFA enrollment first")
			return
		}

		secret, err := mfa.Open(deps.Key, user.ID, user.TOTPSecretEnc)
		if err != nil {
			record("error")
			internalError(w, r, log, "open pending mfa secret", err)
			return
		}
		step, ok := mfa.Validate(secret, req.Code, now())
		if !ok {
			record("invalid_code")
			WriteError(w, r, http.StatusUnauthorized, "invalid_code", invalidCodeMessage)
			return
		}

		codes, err := mfa.RecoveryCodes(mfaRecoveryCodeCount)
		if err != nil {
			record("error")
			internalError(w, r, log, "generate recovery codes", err)
			return
		}
		hashes := make([][]byte, 0, len(codes))
		for _, code := range codes {
			hashes = append(hashes, mfa.HashRecoveryCode(code))
		}

		if err := deps.Store.ConfirmTOTP(r.Context(), user.ID, step, hashes); err != nil {
			switch {
			case errors.Is(err, store.ErrTOTPReplay):
				record("invalid_code")
				WriteError(w, r, http.StatusUnauthorized, "invalid_code", invalidCodeMessage)
			case errors.Is(err, store.ErrMFAAlreadyEnabled):
				WriteError(w, r, http.StatusConflict, "mfa_already_enabled", "this account already has a confirmed second factor")
			case errors.Is(err, store.ErrMFANotEnrolling):
				WriteError(w, r, http.StatusConflict, "mfa_not_enrolling", "start MFA enrollment first")
			default:
				record("error")
				internalError(w, r, log, "confirm mfa", err)
			}
			return
		}

		resp := mfaConfirmResponse{RecoveryCodes: codes}
		if ticket != nil {
			if err := deps.Store.MarkMFATicketUsed(r.Context(), ticket.ID); err != nil {
				if errors.Is(err, store.ErrTicketInvalid) {
					// The factor is confirmed; only this sign-in lost a race for the
					// ticket, so the person signs in again with their new code.
					record("invalid_ticket")
					WriteError(w, r, http.StatusUnauthorized, "invalid_ticket", invalidTicketMessage)
					return
				}
				record("error")
				internalError(w, r, log, "consume mfa ticket", err)
				return
			}
			session, ok := issueSession(w, r, deps.session(), user.ID, user.Name, user.Roles, map[string]any{"mfa": "totp"})
			if !ok {
				record("error")
				return
			}
			resp.AccessToken = session.AccessToken
			resp.ExpiresIn = session.ExpiresIn
			u := session.User
			resp.User = &u
		}

		record("enrolled")
		writeJSON(w, http.StatusOK, resp)
	}
}

// resolveEnrollActor resolves the account enroll or confirm may act for: an enroll
// ticket from login, or the bearer access token of an active account. It writes the
// failure response and returns false; a nil ticket means bearer auth was used.
func (deps MFADeps) resolveEnrollActor(w http.ResponseWriter, r *http.Request, rawTicket string, log *slog.Logger, record func(string)) (store.MFAUser, *store.MFATicket, bool) {
	rawTicket = strings.TrimSpace(rawTicket)

	if rawTicket != "" {
		hash := sha256.Sum256([]byte(rawTicket))
		ticket, err := deps.Store.MFATicketByHash(r.Context(), hash[:])
		if errors.Is(err, store.ErrTicketInvalid) {
			record("invalid_ticket")
			WriteError(w, r, http.StatusUnauthorized, "invalid_ticket", invalidTicketMessage)
			return store.MFAUser{}, nil, false
		}
		if err != nil {
			record("error")
			internalError(w, r, log, "look up mfa enroll ticket", err)
			return store.MFAUser{}, nil, false
		}
		if ticket.Purpose != store.MFAPurposeEnroll {
			record("invalid_ticket")
			WriteError(w, r, http.StatusUnauthorized, "invalid_ticket", invalidTicketMessage)
			return store.MFAUser{}, nil, false
		}
		user, err := deps.Store.MFAUserByID(r.Context(), ticket.UserID)
		if errors.Is(err, store.ErrNotFound) {
			record("invalid_ticket")
			WriteError(w, r, http.StatusUnauthorized, "invalid_ticket", invalidTicketMessage)
			return store.MFAUser{}, nil, false
		}
		if err != nil {
			record("error")
			internalError(w, r, log, "load mfa enroll user", err)
			return store.MFAUser{}, nil, false
		}
		if user.Status != "active" {
			record("invalid_ticket")
			WriteError(w, r, http.StatusUnauthorized, "invalid_ticket", invalidTicketMessage)
			return store.MFAUser{}, nil, false
		}
		return user, &ticket, true
	}

	claims, reason := deps.Verifier.Verify(BearerToken(r))
	if reason != "" {
		record("invalid_ticket")
		WriteError(w, r, http.StatusUnauthorized, reason, token.MessageFor(reason))
		return store.MFAUser{}, nil, false
	}
	user, err := deps.Store.MFAUserByID(r.Context(), claims.Subject)
	if errors.Is(err, store.ErrNotFound) {
		record("invalid_ticket")
		WriteError(w, r, http.StatusUnauthorized, token.ReasonInvalidToken, token.MessageFor(token.ReasonInvalidToken))
		return store.MFAUser{}, nil, false
	}
	if err != nil {
		record("error")
		internalError(w, r, log, "load mfa user", err)
		return store.MFAUser{}, nil, false
	}
	if user.Status != "active" {
		record("invalid_ticket")
		WriteError(w, r, http.StatusUnauthorized, token.ReasonInvalidToken, token.MessageFor(token.ReasonInvalidToken))
		return store.MFAUser{}, nil, false
	}
	return user, nil, true
}

// session builds the shared signing-side deps from this handler's.
func (deps MFADeps) session() sessionDeps {
	return sessionDeps{
		Store:      deps.Store,
		Signer:     deps.Signer,
		AccessTTL:  deps.AccessTTL,
		RefreshTTL: deps.RefreshTTL,
		Logger:     deps.Logger,
	}
}
