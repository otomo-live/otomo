package server

import (
	"log/slog"
	"net/http"

	"github.com/otomo-live/otomo/services/session/internal/api"
	"github.com/otomo-live/otomo/services/session/internal/auth"
)

// guard is the in-service half of COM-4: it verifies the token against the identity
// domain its route belongs to and enforces the route's minimum role before the handler
// runs.
//
// Gateway already did both checks — route authentication is not something a service
// behind a private network may assume happened. 00-common-stack.md §1a is explicit that
// every service re-verifies issuer and audience itself, and §5 of the session spec
// repeats the role check here. The two are not redundant in a way that matters: they
// differ in what they protect. Gateway's check protects the network; this one protects
// the data, and it is the only one that holds if a service is ever called directly by
// something inside the compose network, or if a Gateway route is misconfigured to a
// lower group than the handler needs.
//
// The middleware is applied per route rather than around the mux, because both the
// identity domain and the required role are properties of the route. A single outer
// middleware would either have to re-derive them from the path — duplicating the table
// ServeMux already resolves — or check only the lowest bar and leave every per-route
// boundary to the handlers.
func (s *Server) guard(rt api.Route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Which verifier runs is the whole content of SES-A1: a player route can only
		// ever be satisfied by a player token, and a staff route by a staff token,
		// because the two are checked against different keys, issuers and audiences.
		// Nothing about the request — header, path or body — can move a call from one
		// domain to the other.
		var v *auth.Verifier
		switch rt.Group {
		case api.GroupPlayer:
			v = s.deps.PlayerVerifier
		case api.GroupStaff:
			v = s.deps.StaffVerifier
		}
		if v == nil {
			// A server built with a zero-value Deps — or with a route whose group has no
			// verifier — cannot verify anyone. Answering 500 rather than admitting the
			// request is the whole point of the check: "no verifier configured" must
			// never read as "no authentication required". Only a programming error
			// reaches this, so it is logged as one.
			s.log.LogAttrs(r.Context(), slog.LevelError, "no token verifier configured for this route group; refusing the request",
				slog.String("group", rt.Group.String()),
				slog.String("route", routeLabel(r)),
				slog.String("request_id", api.RequestID(r.Context())),
			)
			api.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}

		id, reason := v.Verify(r.Context(), auth.BearerToken(r))
		// A player route's minimum is RoleNone, so this can only fail on a staff route.
		// A player identity carries no roles by construction, which means a player token
		// that somehow satisfied a staff route's JWKS checks would still be refused
		// here rather than granted viewer access.
		if reason == "" && !id.HasRoleAtLeast(rt.MinRole) {
			reason = auth.ReasonInsufficientRole
		}

		if reason != "" {
			// 401 and 403 are not interchangeable here. A missing, expired, wrong-domain
			// or otherwise unusable token is 401: the caller may fix it by presenting a
			// different one. A well-formed, correctly-issued token that simply does not
			// carry the role this route needs is 403: presenting it again changes
			// nothing, and the client should stop retrying and tell the person their
			// account is not allowed to do this.
			status := http.StatusUnauthorized
			if reason == auth.ReasonInsufficientRole {
				status = http.StatusForbidden
			}

			// The reason is both the metric label and the response body's code, so the
			// 401 a client sees and the spike on a dashboard are the same string. No log
			// line of its own: the access log already records the reason for every
			// request, and logging twice would put two lines at different levels for one
			// event without either being more true.
			s.metrics.tokenRejected.WithLabelValues(string(v.Domain()), reason).Inc()
			setReason(r.Context(), reason)
			api.WriteError(w, r, status, reason, auth.MessageFor(reason))
			return
		}

		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
	})
}
