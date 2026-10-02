package server

import (
	"log/slog"
	"net/http"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/auth"
)

// guard is the in-service half of COM-4: it verifies the staff token and enforces the
// route's minimum role before the handler runs.
//
// Gateway already did both checks — route authentication is not something a service
// behind a private network may assume happened. 00-common-stack.md §1a is explicit
// that every service re-verifies issuer and audience itself, and §5 of the config spec
// repeats the role check here. The two are not redundant in a way that matters: they
// differ in what they protect. Gateway's check protects the network; this one protects
// the data, and it is the only one that holds if a service is ever called directly by
// something inside the compose network, or if a Gateway route is misconfigured to a
// lower group than the handler needs.
//
// The middleware is applied per route rather than around the mux, because the required
// role is a property of the route. A single outer middleware would either have to
// re-derive the role from the path — duplicating the table that ServeMux already
// resolves — or check only the lowest bar and leave every per-route boundary to the
// handlers.
func (s *Server) guard(rt api.Route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := s.deps.Verifier
		if v == nil {
			// A server built with a zero-value Deps cannot verify anyone. Answering
			// 500 rather than admitting the request is the whole point of the check:
			// "no verifier configured" must never read as "no authentication
			// required". Only a programming error reaches this — main always supplies
			// one — so it is logged as such.
			s.log.LogAttrs(r.Context(), slog.LevelError, "no token verifier configured; refusing the request",
				slog.String("route", routeLabel(r)),
				slog.String("request_id", api.RequestID(r.Context())),
			)
			api.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}

		claims, reason := v.Verify(r.Context(), auth.BearerToken(r))
		if reason == "" && !claims.HasRoleAtLeast(rt.MinRole) {
			reason = auth.ReasonInsufficientRole
		}

		if reason != "" {
			// 401 and 403 are not interchangeable here. A missing, expired or
			// otherwise unusable token is 401: the caller may fix it by presenting a
			// different one. A well-formed, correctly-issued token that simply does not
			// carry the role this route needs is 403: presenting it again changes
			// nothing, and the client should stop retrying and tell the person their
			// account is not allowed to do this. CFG-A1's acceptance criterion makes
			// the same split for the player-token and missing-token cases.
			status := http.StatusUnauthorized
			if reason == auth.ReasonInsufficientRole {
				status = http.StatusForbidden
			}

			// The reason is both the metric label and the response body's code, so the
			// 403 a staff member sees and the spike on a dashboard are the same string.
			// No log line of its own: the access log already records the reason for
			// every request, and logging twice would put two lines at different levels
			// for one event without either being more true.
			s.metrics.tokenRejected.WithLabelValues(reason).Inc()
			setReason(r.Context(), reason)
			api.WriteError(w, r, status, reason, auth.MessageFor(reason))
			return
		}

		next.ServeHTTP(w, r.WithContext(auth.WithClaims(r.Context(), claims)))
	})
}
