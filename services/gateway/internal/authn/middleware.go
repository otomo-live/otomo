package authn

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"gateway/gateway/internal/apierr"
	"gateway/gateway/internal/obslog"
	"gateway/gateway/internal/router"
)

// Rejection reasons. These are the values of
// gateway_token_rejected_total{reason} (techspec §8) and, at the same time, the
// COM-5 error code in the response body, so an operator reading a client's 401
// and an operator reading the metric are looking at the same string.
const (
	reasonMissingToken     = "missing_token"
	reasonInvalidSignature = "invalid_signature"
	reasonExpired          = "expired"
	reasonAudienceMismatch = "aud_mismatch"
	reasonIssuerMismatch   = "iss_mismatch"
	reasonInsufficientRole = "insufficient_role"
	// reasonInvalidToken is the fallback for a token that is well-formed JSON
	// but fails for a reason outside §8's closed list: malformed structure, a
	// missing subject, a not-yet-valid `nbf`, an unparseable signing method, or
	// a domain whose keys have not been fetched yet.
	reasonInvalidToken = "invalid_token"
)

// Middleware wraps a route's handler with authentication. The shape matches what
// route registration needs: given the route and the handler it would otherwise
// serve, return the handler to register.
type Middleware func(router.Route, http.Handler) http.Handler

// RequireGroup returns the authentication middleware for a route table.
//
// A GroupPublic route is returned untouched — those are how a client gets a
// token in the first place, so they cannot require one (techspec §9).
func RequireGroup(reg *Registry, issuers map[router.Group]Issuer, clockSkew time.Duration) Middleware {
	return func(route router.Route, next http.Handler) http.Handler {
		if route.Group == router.GroupPublic {
			return next
		}

		iss, ok := issuers[route.Group]
		if !ok || iss.Issuer == "" || iss.Audience == "" {
			// Startup validation (verifyIssuers) makes this unreachable in main.
			// If a route table is ever built without it, fail closed and loudly
			// rather than serving a protected route with no check at all.
			slog.Error("route has no configured issuer; refusing to serve it",
				"route", route.MuxPattern(), "group", route.Group.String())
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				apierr.WriteError(w, r, http.StatusInternalServerError, "internal_error",
					"route authentication is misconfigured")
			})
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, reason := validate(reg, r, route, iss, clockSkew)
			if reason != "" {
				obslog.TokenRejected.WithLabelValues(reason, route.Group.String()).Inc()
				// Surfaced on the access log line, which is what makes a 401
				// debuggable from the Dashboard without reading raw logs.
				obslog.SetReason(r.Context(), reason)
				status := http.StatusUnauthorized
				if reason == reasonInsufficientRole {
					status = http.StatusForbidden
				}
				apierr.WriteError(w, r, status, reason, messageFor(reason))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithClaims(r.Context(), claims)))
		})
	}
}

// validate checks one request's bearer token and returns either the verified
// claims or the reason it was rejected.
func validate(reg *Registry, r *http.Request, route router.Route, iss Issuer, clockSkew time.Duration) (*Claims, string) {
	tokenStr := bearerToken(r)
	if tokenStr == "" {
		return nil, reasonMissingToken
	}

	// A domain whose keys have not arrived yet cannot verify anything. This is
	// the state a gateway is in between starting and Auth's first JWKS response.
	kf := reg.Keyfunc(route.Group)
	if kf == nil {
		return nil, reasonInvalidToken
	}

	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims,
		// KeyfuncCtx, not the bare Keyfunc: the context-aware variant carries the
		// request's deadline into the re-fetch that an unrecognized `kid` triggers,
		// so a token with a random kid cannot pin a request thread for longer than
		// the client is willing to wait.
		kf.KeyfuncCtx(r.Context()),
		// Pinning the algorithm is what prevents an algorithm-substitution attack.
		// Without it, ParseWithClaims trusts whatever `alg` the token header
		// names and asks the keyfunc for a matching key.
		jwt.WithValidMethods([]string{"EdDSA"}),
		// iss and aud are checked against the token's own claims, not inferred
		// from which JWKS answered. This is what makes a player token on a staff
		// route fail even when both domains happen to share a key.
		jwt.WithIssuer(iss.Issuer),
		jwt.WithAudience(iss.Audience),
		jwt.WithLeeway(clockSkew),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !token.Valid {
		return nil, reasonFor(err)
	}

	// `sub` is the key every downstream service hangs data off: Session's whole
	// model is keyed on it (contract §4, "the one item in this whole document
	// most likely to cause a real bug if got wrong"). A token without one would
	// silently create a ghost profile downstream, so it is rejected here, at the
	// only place that can tell the difference.
	if claims.Subject == "" {
		return nil, reasonInvalidToken
	}

	if route.MinRole > 0 && !claims.HasRoleAtLeast(route.MinRole) {
		return nil, reasonInsufficientRole
	}

	return claims, ""
}

// reasonFor maps a parse failure onto one of §8's rejection reasons.
//
// Order matters, in two places.
//
// Signature and key-resolution failures come first: a forged token's other
// claims are meaningless, so labelling it "expired" because it also happens to
// be stale would point an operator at the wrong cause.
//
// Issuer is then tested before audience. A token from the wrong domain is wrong
// on both claims at once, and the issuer is the one that names *which* domain it
// came from — the more useful of the two when someone is staring at
// gateway_token_rejected_total trying to work out why an admin call 401s.
func reasonFor(err error) string {
	switch {
	case err == nil:
		return reasonInvalidToken
	case errors.Is(err, jwt.ErrTokenSignatureInvalid), errors.Is(err, jwt.ErrTokenUnverifiable):
		return reasonInvalidSignature
	case errors.Is(err, jwt.ErrTokenExpired):
		return reasonExpired
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return reasonIssuerMismatch
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return reasonAudienceMismatch
	default:
		return reasonInvalidToken
	}
}

// messageFor is the human-readable half of the COM-5 body. Deliberately vague
// about which key or claim disagreed: the reason code already carries what an
// operator needs, and a client does not need to be told how to forge a better
// token.
func messageFor(reason string) string {
	switch reason {
	case reasonMissingToken:
		return "an access token is required"
	case reasonExpired:
		return "the access token has expired"
	case reasonAudienceMismatch, reasonIssuerMismatch:
		return "the access token was not issued for this service"
	case reasonInsufficientRole:
		return "the access token does not carry a sufficient role"
	default:
		return "the access token is not valid"
	}
}

// bearerToken returns the token from the Authorization header, or "" when the
// header is absent or not a bearer credential.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}
