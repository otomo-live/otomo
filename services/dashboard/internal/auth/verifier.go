package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// fetchTimeout bounds one JWKS request. Without it the library's own default is a
// minute, which is long enough for a half-open connection to stall start-up.
const fetchTimeout = 5 * time.Second

// Rejection reasons. These are the values of dashboard_token_rejected_total{reason} and,
// at the same time, the COM-5 error code in the response body, so an operator reading
// a client's 401 and an operator reading the metric are looking at the same string.
// The set is deliberately identical to Gateway's, so a request that Gateway admitted
// and Config refused names its cause the same way in both services' logs.
const (
	ReasonMissingToken     = "missing_token"
	ReasonInvalidSignature = "invalid_signature"
	ReasonExpired          = "expired"
	ReasonAudienceMismatch = "aud_mismatch"
	ReasonIssuerMismatch   = "iss_mismatch"
	ReasonInsufficientRole = "insufficient_role"
	// ReasonInvalidToken is the fallback for a token that is well-formed JSON but
	// fails for a reason outside that closed list: malformed structure, a missing
	// subject, a not-yet-valid `nbf`, or an unparseable signing method.
	//
	// A JWKS that has not been fetched yet is *not* one of those, which is worth
	// stating because it is the intuitive guess: with the client built but its key set
	// empty, a `kid` resolves to nothing, jwt reports that as an unverifiable token,
	// and ReasonFor calls it invalid_signature. Both are 401; only the code differs,
	// and invalid_signature is the more honest of the two — nothing was verified.
	// ReasonInvalidToken does cover the other half of the same situation, the window
	// before the client exists at all, where Verify rejects without parsing.
	ReasonInvalidToken = "invalid_token"
)

// Verifier checks staff tokens against PHP Admin Auth's JWKS.
//
// The JWKS client is built once and lives for the process. That is deliberate:
// keyfunc starts its background refresh goroutine before performing the first fetch,
// so constructing a client per retry would leave one goroutine behind for every
// failed attempt. Instead the client is built once, the library's ticker is the
// retry, and readiness is derived from whether any key has arrived yet.
type Verifier struct {
	url      string
	issuer   string
	audience string
	refresh  time.Duration
	skew     time.Duration

	// kf is nil until the first fetch completes. A nil client rejects everything,
	// which is the correct direction to be wrong in for a process that has not yet
	// seen any public key.
	kf atomic.Pointer[keyfunc.Keyfunc]
}

// NewVerifier returns a Verifier for one staff identity domain. It does not fetch
// anything; call Start.
func NewVerifier(url, issuer, audience string, refresh, skew time.Duration) *Verifier {
	return &Verifier{url: url, issuer: issuer, audience: audience, refresh: refresh, skew: skew}
}

// URL is the JWKS endpoint this verifier was built on. It exists so a readiness
// message can name the endpoint that has not answered yet — "staff jwks not fetched"
// on its own sends an operator looking through the environment for which one.
func (v *Verifier) URL() string {
	return v.url
}

// Start begins fetching the keys. It returns immediately: the service must be able to
// listen whether or not PHP Admin Auth is up yet, and an unavailable JWKS endpoint
// must never keep the process from starting. Until keys arrive, every request is
// rejected and /readyz reports 503 — which is the honest description of a service
// that cannot verify anyone.
//
// The passed context ends the background refresh. Cancelling it does not close the
// listeners and is only expected at shutdown or in tests.
func (v *Verifier) Start(ctx context.Context) {
	go func() {
		kf, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{v.url}, keyfunc.Override{
			HTTPTimeout: fetchTimeout,
			// NoErrorReturnFirstHTTPReq stays at its default of true, so an
			// unreachable endpoint yields a working-but-empty client rather than an
			// error. keyfunc's ticker then keeps retrying until the service appears.
			RefreshInterval: v.refresh,
			RefreshErrorHandlerFunc: func(string) func(context.Context, error) {
				return func(ctx context.Context, err error) {
					// Warn, not Error: this fires every refresh interval for as long
					// as PHP Admin Auth is not deployed, and Error there would be
					// noise an operator learns to ignore. /readyz is the signal that
					// says whether it actually matters.
					slog.WarnContext(ctx, "staff jwks refresh failed", "url", v.url, "err", err)
				}
			},
		})
		if err != nil {
			// Only reachable for a malformed URL or an unparseable key set, not for
			// an unavailable host. This verifier can never admit anyone; the process
			// keeps serving, and /readyz stays 503.
			slog.ErrorContext(ctx, "cannot build the staff jwks client; every token will be rejected",
				"url", v.url, "err", err)
			return
		}
		v.kf.Store(&kf)
		slog.InfoContext(ctx, "staff jwks client started", "url", v.url, "issuer", v.issuer)
	}()
}

// Ready reports whether at least one key has been fetched. A refresh that failed
// after a successful one keeps the previous keys and stays ready — keyfunc only
// replaces the set on a successful fetch — which is what lets this service keep
// verifying tokens while PHP Admin Auth is briefly down.
func (v *Verifier) Ready(ctx context.Context) bool {
	kf := v.kf.Load()
	if kf == nil {
		return false
	}
	keys, err := (*kf).Storage().KeyReadAll(ctx)
	return err == nil && len(keys) > 0
}

// Verify checks one bearer token and returns either the verified claims or the reason
// it was rejected. A "" reason means the token was accepted.
//
// The context is the request's, so the re-fetch an unrecognized `kid` triggers is
// bounded by the client's own deadline: a token with a random kid cannot hold a
// request thread open longer than the caller is willing to wait.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, string) {
	if token == "" {
		return nil, ReasonMissingToken
	}

	kf := v.kf.Load()
	if kf == nil {
		return nil, ReasonInvalidToken
	}

	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims,
		(*kf).KeyfuncCtx(ctx),
		// Pinning the algorithm is what prevents an algorithm-substitution attack:
		// without it, ParseWithClaims trusts whatever `alg` the token header names
		// and asks the keyfunc for a matching key.
		jwt.WithValidMethods([]string{"EdDSA"}),
		// iss and aud are checked against the token's own claims, not inferred from
		// which JWKS answered. That is what stops a player token from being accepted
		// here even if both domains ever shared a key.
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithLeeway(v.skew),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !parsed.Valid {
		return nil, ReasonFor(err)
	}

	// `sub` is the identity every downstream decision hangs off — the audit log's
	// actor_id, and the only stable handle PHP's staff table has for this person. A
	// token without one would write audit rows nobody can attribute, so it is
	// rejected here, at the only place that can tell the difference.
	if claims.Subject == "" {
		return nil, ReasonInvalidToken
	}

	return claims, ""
}

// ReasonFor maps a parse failure onto one of the reasons above.
//
// Order matters, in two places.
//
// Signature and key-resolution failures come first: a forged token's other claims are
// meaningless, so labelling it "expired" because it also happens to be stale would
// point an operator at the wrong cause.
//
// Issuer is then tested before audience. A token from another domain is wrong on both
// claims at once, and the issuer is the one that names *which* domain it came from —
// the more useful of the two when someone is staring at
// dashboard_token_rejected_total trying to work out why an admin call 401s.
func ReasonFor(err error) string {
	switch {
	case err == nil:
		return ReasonInvalidToken
	case errors.Is(err, jwt.ErrTokenSignatureInvalid), errors.Is(err, jwt.ErrTokenUnverifiable):
		return ReasonInvalidSignature
	case errors.Is(err, jwt.ErrTokenExpired):
		return ReasonExpired
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return ReasonIssuerMismatch
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return ReasonAudienceMismatch
	default:
		return ReasonInvalidToken
	}
}

// MessageFor is the human-readable half of the COM-5 body. Deliberately vague about
// which key or claim disagreed: the reason code already carries what an operator
// needs, and a client does not need to be told how to forge a better token.
func MessageFor(reason string) string {
	switch reason {
	case ReasonMissingToken:
		return "an access token is required"
	case ReasonExpired:
		return "the access token has expired"
	case ReasonAudienceMismatch, ReasonIssuerMismatch:
		return "the access token was not issued for this service"
	case ReasonInsufficientRole:
		return "the access token does not carry a sufficient role"
	default:
		return "the access token is not valid"
	}
}

// BearerToken returns the token from the Authorization header, or "" when the header
// is absent or not a bearer credential.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}
