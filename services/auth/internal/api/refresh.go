// POST /auth/refresh and POST /auth/logout — the refresh-token half of a session.
//
// The Godot client is not a browser, so the refresh token travels in the JSON body,
// never in a cookie. Every unusable refresh token — missing, malformed, unknown,
// expired, revoked or replayed — gets the same 401 invalid_token: telling a caller
// which one it was would be an oracle, and the client's response is the same for all
// of them (log in again via /auth/anonymous, never refresh again).

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"time"

	"github.com/otomo-live/otomo/services/auth/internal/store"
)

// refreshTokenBytes is the entropy of a refresh token: 256 bits from crypto/rand,
// sent as 43 base64url characters.
const refreshTokenBytes = 32

// RefreshStore is the subset of *store.DB the refresh-token handlers need.
type RefreshStore interface {
	InsertRefreshToken(ctx context.Context, accountID string, tokenHash []byte, expiresAt time.Time) (string, error)
	RotateRefreshToken(ctx context.Context, in store.RotateInput) (store.RotateResult, error)
	RevokeRefreshFamily(ctx context.Context, tokenHash []byte) error
}

// RefreshDeps is everything the refresh handler needs.
type RefreshDeps struct {
	Refresh    RefreshStore
	Issuer     TokenIssuer
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Logger     *slog.Logger
	Outcomes   Outcomes // may be nil
}

// refreshRequest is the body of both /auth/refresh and /auth/logout. RefreshToken is
// a secret and is never logged.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Refresh returns the POST /auth/refresh handler.
func Refresh(deps RefreshDeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		if err := decodeBody(w, r, &req); err != nil {
			writeValidationFailed(w, r, `body must be {"refresh_token": "<token>"}`)
			countRefresh(deps.Outcomes, ResultInvalid)
			return
		}
		hash, ok := refreshTokenHash(req.RefreshToken)
		if !ok {
			writeInvalidRefreshToken(w, r)
			countRefresh(deps.Outcomes, ResultInvalid)
			return
		}
		fail := func(what string, err error) {
			internalError(w, r, log, what, err)
			countRefresh(deps.Outcomes, ResultError)
		}

		successor, successorHash, err := newRefreshToken()
		if err != nil {
			fail("mint refresh token", err)
			return
		}

		now := time.Now()
		res, err := deps.Refresh.RotateRefreshToken(r.Context(), store.RotateInput{
			TokenHash:    hash,
			NewTokenHash: successorHash,
			TTL:          deps.RefreshTTL,
			Now:          now,
		})
		if err != nil {
			fail("rotate refresh token", err)
			return
		}
		if res.Outcome != store.RotateRotated {
			switch res.Outcome {
			case store.RotateReused:
				countRefresh(deps.Outcomes, ResultReuseDetected)
			case store.RotateRevoked:
				countRefresh(deps.Outcomes, ResultRevoked)
			default:
				countRefresh(deps.Outcomes, ResultInvalid)
			}
			if res.Outcome == store.RotateReused {
				// The one outcome that is a security signal rather than routine
				// expiry: an exchanged token came back. The IDs let an operator find
				// the session; the token itself is never logged.
				log.LogAttrs(r.Context(), slog.LevelWarn, "refresh_token_reuse",
					slog.String("account_id", res.AccountID),
					slog.String("family_id", res.FamilyID),
					slog.String("request_id", RequestID(r.Context())),
				)
			}
			writeInvalidRefreshToken(w, r)
			return
		}

		access, err := deps.Issuer.Issue(res.AccountID, now)
		if err != nil {
			fail("issue access token", err)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, newLoginResponse(access, deps.AccessTTL, successor))
		countRefresh(deps.Outcomes, ResultOK)
	}
}

// LogoutDeps is everything the logout handler needs.
type LogoutDeps struct {
	Refresh RefreshStore
	Logger  *slog.Logger
}

// Logout returns the POST /auth/logout handler. It revokes the presented token's whole
// family and answers 204 whatever the body held — a malformed body, an unknown or an
// already-revoked token all end in the same state, so a logout that reported "nothing
// to do" would only give a client something to branch on. Only a database failure,
// where the revocation may not have happened, is not a 204.
func Logout(deps LogoutDeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		if err := decodeBody(w, r, &req); err == nil {
			if hash, ok := refreshTokenHash(req.RefreshToken); ok {
				if err := deps.Refresh.RevokeRefreshFamily(r.Context(), hash); err != nil {
					internalError(w, r, log, "revoke refresh family", err)
					return
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// newRefreshToken returns a fresh refresh token and the hash that is stored for it.
func newRefreshToken() (string, []byte, error) {
	raw := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), sum[:], nil
}

// refreshTokenHash decodes a presented refresh token and returns its stored hash. The
// second result is false for anything that could not have been issued by
// newRefreshToken, which saves a database round trip for garbage.
func refreshTokenHash(token string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != refreshTokenBytes {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}

// writeInvalidRefreshToken is the one 401 for every unusable refresh token. No
// WWW-Authenticate header: the client is not a browser and the code says everything.
func writeInvalidRefreshToken(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusUnauthorized, "invalid_token",
		"the refresh token is not valid; log in again")
}

// writeValidationFailed is the 400 for a body that is not the documented JSON shape.
func writeValidationFailed(w http.ResponseWriter, r *http.Request, message string) {
	WriteError(w, r, http.StatusBadRequest, "validation_failed", message)
}
