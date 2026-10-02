// POST /admin-auth/login — the staff password login.
//
// The response contract is fixed by design/06 §13 and the admin UI is coded against it,
// so the shapes here are not free to drift. Every credential failure — unknown email,
// wrong password, disabled account, locked account — is one identical 401, because
// telling a caller which of those it was is an account-enumeration oracle. The
// unknown-email path deliberately verifies against a fixed dummy hash so it costs the
// same as a real lookup.

package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"uuid"

	"github.com/otomo-live/otomo/services/admin_auth/internal/password"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

const (
	// maxLoginBody bounds the request body. The contract makes anything over 16 KiB a
	// 400, so the limit is a constant of the endpoint rather than a tuning knob.
	maxLoginBody = 16 << 10

	// refreshCookieName and refreshCookiePath are the cookie's identity. The __Host-
	// prefix is required because the host may share a parent domain with sites otomo
	// doesn't control (a school's or company's subdomain): without it a sibling subdomain could plant or overwrite a cookie named
	// otomo_refresh for the shared parent (cookie tossing) and feed the auth service
	// a refresh token it never issued. Browsers only accept __Host- when the cookie
	// is Secure, carries no Domain attribute, and uses Path=/, so the path cannot
	// stay narrow: a non-root Path and the prefix are mutually exclusive. The prefix
	// scopes the cookie to this exact host, and gateway_dev forwards cookies only on
	// the admin-auth routes (Route.ForwardCookies), so no other service is sent it.
	refreshCookieName = "__Host-otomo_refresh"
	refreshCookiePath = "/"

	// refreshTokenBytes is the entropy in a refresh token before base64 encoding.
	refreshTokenBytes = 32

	// maxUserAgent is how much of the User-Agent is retained on the session row. A
	// client controls this header, so it is truncated to keep a hostile one from
	// bloating the table.
	maxUserAgent = 256

	// invalidCredentialsMessage is identical for every credential failure. Do not make
	// it more specific.
	invalidCredentialsMessage = "that email and password do not match an account"
)

// LoginStore is the subset of *store.DB the login handler needs. It is an interface so
// the handler can be tested without a database and so the server package owns the
// concrete wiring.
type LoginStore interface {
	UserByEmail(ctx context.Context, email string) (store.User, error)
	RecordLoginFailure(ctx context.Context, id string, maxFailures int, lockFor time.Duration) (bool, error)
	RecordLoginSuccess(ctx context.Context, id, ip, ua string, refreshHash []byte, familyID uuid.UUID, refreshTTL time.Duration, actor store.Entry, details map[string]any) error
	CreateMFATicket(ctx context.Context, userID, purpose string, ttl time.Duration) (string, error)
}

// LoginDeps is everything the login handler needs. Verify is an injection point for
// tests: when nil the handler uses password.Verify, which is what production does.
type LoginDeps struct {
	Store       LoginStore
	Signer      *token.Signer
	AccessTTL   time.Duration
	RefreshTTL  time.Duration
	MaxFailures int
	Lockout     time.Duration
	Logger      *slog.Logger
	Verify      func(phc, pw string) (bool, error)
	// OnResult, when non-nil, is called once per request with the outcome label the
	// server records in its metrics. The api package stays free of Prometheus: the
	// server wraps its counter in this closure.
	OnResult func(result string)
}

// loginRequest is the request body. Unknown fields are rejected by the decoder, so
// this is also the whole accepted shape.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// loginResponse is the 200 body: the access token, its lifetime in seconds, and the
// identity the token describes.
type loginResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresIn   int64     `json:"expires_in"`
	User        loginUser `json:"user"`
}

type loginUser struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

// Login returns the POST /admin-auth/login handler.
func Login(deps LoginDeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	verify := deps.Verify
	if verify == nil {
		verify = password.Verify
	}
	// record is the one place the outcome metric is incremented; every return path
	// calls it exactly once. It is nil-safe so a test that builds LoginDeps by hand
	// does not have to supply a counter.
	record := func(result string) {
		if deps.OnResult != nil {
			deps.OnResult(result)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if !decodeLoginBody(w, r, &req) {
			record("bad_request")
			return
		}
		req.Email = strings.TrimSpace(req.Email)
		if req.Email == "" || req.Password == "" {
			record("bad_request")
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "email and password must not be empty")
			return
		}

		user, err := deps.Store.UserByEmail(r.Context(), req.Email)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Same work as a found user, then the same answer.
				_, _ = verify(password.Dummy(), req.Password)
				record("invalid_credentials")
				WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", invalidCredentialsMessage)
				return
			}
			record("error")
			internalError(w, r, log, "look up staff user", err)
			return
		}

		// Verify before any status or lock check: whether the account can log in must
		// not change how much work the request does, and a disabled or locked account
		// still runs the hash before it is refused.
		ok, err := verify(user.PasswordHash, req.Password)
		if err != nil {
			record("error")
			internalError(w, r, log, "verify staff password", err)
			return
		}

		if user.Status != "active" {
			record("invalid_credentials")
			WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", invalidCredentialsMessage)
			return
		}
		now := time.Now()
		if user.LockedUntil != nil && user.LockedUntil.After(now) {
			// Already locked: this attempt does not count against the next window and
			// must not extend the lock.
			record("locked")
			WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", invalidCredentialsMessage)
			return
		}
		if !ok {
			locked, err := deps.Store.RecordLoginFailure(r.Context(), user.ID, deps.MaxFailures, deps.Lockout)
			if err != nil {
				record("error")
				internalError(w, r, log, "record login failure", err)
				return
			}
			if locked {
				record("locked")
			} else {
				record("invalid_credentials")
			}
			WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", invalidCredentialsMessage)
			return
		}

		// The password, the account status and the lockout are all clear. The
		// shared post-password policy decides between a challenge and a session:
		// root is exempt, a confirmed factor must be proved, an admin without one
		// must enroll. Failures above never count against a password that was
		// right, so the counter only resets when a session is actually issued.
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
		}, record)
	}
}

// decodeLoginBody reads the one JSON object the body must contain: at most 16 KiB, no
// unknown fields, and nothing after the object. Any violation is a 400 invalid_body,
// and the handler is expected to return immediately when this returns false.
func decodeLoginBody(w http.ResponseWriter, r *http.Request, dst *loginRequest) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body is not a single valid JSON object")
		return false
	}
	// A second Decode must report EOF; anything else is trailing data or a second
	// value, which the contract calls malformed.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body is not a single valid JSON object")
		return false
	}
	return true
}

// newRefreshToken returns 32 random bytes as unpadded base64url. The raw value is the
// cookie and is never stored; only its sha256 reaches the database.
func newRefreshToken() (string, error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// clientIP is the address recorded on the refresh session. Gateway terminates the
// connection and sets X-Forwarded-For to the peer, so that header is preferred; when
// the service is reached directly the RemoteAddr host is used instead.
// maxClientIP bounds the stored address; an IPv6 literal is at most 45 bytes.
const maxClientIP = 64

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		return truncateBytes(strings.TrimSpace(xff), maxClientIP)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// truncateBytes caps s at n bytes and returns valid UTF-8. Both halves matter: the
// value is client-controlled and lands in a Postgres text column, which rejects
// invalid UTF-8 — so a raw cut through a multi-byte rune, or a header that was never
// UTF-8, would turn a correct login into a 500.
func truncateBytes(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}
