// POST /auth/anonymous — device login. The client sends the device_id it generated on
// first launch; the same device_id always resolves to the same account, and so to the
// same `sub`.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/otomo-live/otomo/services/auth/internal/store"
)

// maxLoginBody bounds the request body. A valid body is well under 200 bytes; the cap
// only has to be generous enough that no honest client ever meets it.
const maxLoginBody = 4 << 10

// deviceIDPattern is the whole of device_id validation. The SDK generates 32 random
// bytes as base64url (43 characters); the range leaves room for other generators
// without admitting anything that is not URL-safe.
var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22,128}$`)

// AccountStore is the subset of *store.DB the login handler needs. created reports
// whether this call made the account, for auth_logins_total{new_account}.
type AccountStore interface {
	FindOrCreateAccount(ctx context.Context, method, externalID string) (id string, created bool, err error)
}

// TokenIssuer signs an access token for a subject. *token.Signer satisfies it.
type TokenIssuer interface {
	Issue(sub string, now time.Time) (string, error)
}

// AnonymousDeps is everything the device-login handler needs.
type AnonymousDeps struct {
	Accounts   AccountStore
	Refresh    RefreshStore
	Issuer     TokenIssuer
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Logger     *slog.Logger
	Outcomes   Outcomes // may be nil
}

// anonymousRequest is the request body. DeviceID is a secret: it is the player's only
// credential, so it is never logged and never stored in the clear.
type anonymousRequest struct {
	DeviceID string `json:"device_id"`
}

// schemaVersion versions the whole login/refresh body (06 §12, AUTH-8.4). It changes
// only for a change a client written against the previous version would misread.
// Dropping the "services" hand-off (D2) was not one: every client already fell back to
// {gateway}/api/player/session when it was missing.
const schemaVersion = 1

// loginResponse is the success body of /auth/anonymous and /auth/refresh: one shape,
// so the client has one parser for both. Build it with newLoginResponse.
//
// It carries tokens only. Auth does not tell a client where to go next (D2, settled
// 2026-09-29): the client knows the gateway's base URL and the fixed path of each
// service behind it (06 §12), and learns a game server's address from Session's
// party.launching event when its lobby launches (design/14-launch-handoff.md §3, §7).
type loginResponse struct {
	SchemaVersion int    `json:"schema_version"`
	AccessToken   string `json:"access_token"`
	ExpiresIn     int64  `json:"expires_in"`
	RefreshToken  string `json:"refresh_token"`
}

func newLoginResponse(access string, accessTTL time.Duration, refresh string) loginResponse {
	return loginResponse{
		SchemaVersion: schemaVersion,
		AccessToken:   access,
		ExpiresIn:     int64(accessTTL.Seconds()),
		RefreshToken:  refresh,
	}
}

// Anonymous returns the POST /auth/anonymous handler.
func Anonymous(deps AnonymousDeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req anonymousRequest
		if err := decodeBody(w, r, &req); err != nil || !deviceIDPattern.MatchString(req.DeviceID) {
			// One code and one message for every bad body: which check failed is not
			// something a well-behaved client needs, and the SDK generates the value.
			writeValidationFailed(w, r,
				`body must be {"device_id": <22-128 characters of A-Z a-z 0-9 _ ->}`)
			countLogin(deps.Outcomes, ResultInvalid, false)
			return
		}

		accountID, created, err := deps.Accounts.FindOrCreateAccount(r.Context(), store.MethodDevice, deviceExternalID(req.DeviceID))
		if err != nil {
			internalError(w, r, log, "find or create device account", err)
			countLogin(deps.Outcomes, ResultError, false)
			return
		}
		fail := func(what string, err error) {
			internalError(w, r, log, what, err)
			countLogin(deps.Outcomes, ResultError, created)
		}

		now := time.Now()
		access, err := deps.Issuer.Issue(accountID, now)
		if err != nil {
			fail("issue access token", err)
			return
		}

		// Every login starts a new refresh family, so logging out on one install
		// never ends a session another install of the same device_id holds.
		refresh, refreshHash, err := newRefreshToken()
		if err != nil {
			fail("mint refresh token", err)
			return
		}
		if _, err := deps.Refresh.InsertRefreshToken(r.Context(), accountID, refreshHash, now.Add(deps.RefreshTTL)); err != nil {
			fail("insert refresh token", err)
			return
		}

		// Token responses must never be cached by anything between here and the client.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, newLoginResponse(access, deps.AccessTTL, refresh))
		countLogin(deps.Outcomes, ResultOK, created)
	}
}

// deviceExternalID is the identity_binding.external_id for a device login: the hex
// SHA-256 of the device_id. A fast hash is enough because a valid device_id carries at
// least 128 bits of entropy (22 base64url characters); there is nothing to brute-force.
func deviceExternalID(deviceID string) string {
	sum := sha256.Sum256([]byte(deviceID))
	return hex.EncodeToString(sum[:])
}

// decodeBody decodes exactly one JSON value from a size-capped body into dst. Trailing
// data after the value is an error, so a body that is two concatenated objects cannot
// be half-read.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBody))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON body")
	}
	return nil
}
