package server

import (
	"context"
	"net/http"
	"strconv"

	"github.com/otomo-live/otomo/services/session/internal/api"
	"github.com/otomo-live/otomo/services/session/internal/rules"
)

// ReleaseHead is where the release check reads the live channel head's release_id.
// *rules.Loader implements it.
type ReleaseHead interface {
	Head() int64
	Refresh(ctx context.Context) int64
}

// Results of the release check, the values of session_release_checks_total{result}.
const (
	releaseCurrent   = "current"   // the client's release is the head
	releaseOutdated  = "outdated"  // any other release: 409 release_outdated
	releaseMissing   = "missing"   // no header: let through unless required
	releaseInvalid   = "invalid"   // a header that is not a release_id: 400
	releaseUnchecked = "unchecked" // the head is not known yet: let through
)

// releaseCheck is content release enforcement (SE-8, decision D5 in
// design/13-player-plane-plan.md), wrapped around every player route inside the token
// guard, so a caller without a valid token still gets 401 first. Staff routes do not
// pass through it.
//
// The client's X-Otomo-Release must equal the head's release_id. Equality, not "at
// least": a rollback moves the head back to a lower id, and a client still on the
// rolled-back release has to patch too. On a mismatch the head is refreshed once
// (Refresh polls Patch at most every few seconds and never waits for another poll), so
// a client that patched to a release published since the last poll passes.
//
// A request without the header passes while cfg.RequireReleaseHeader is false, which it
// is until the SDK sends the header. While the head is unknown (no Patch key,
// or Patch has not answered yet) every request passes: refusing them all would take the
// game down with Patch.
func (s *Server) releaseCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := s.checkRelease(r)
		s.metrics.releaseChecks.WithLabelValues(result).Inc()
		switch result {
		case releaseOutdated:
			api.WriteError(w, r, http.StatusConflict, "release_outdated",
				"your content release is not the live one; update the game content and try again")
			return
		case releaseInvalid:
			api.WriteError(w, r, http.StatusBadRequest, "invalid_release",
				rules.ReleaseHeader+" must be a release_id")
			return
		case releaseMissing:
			if s.cfg.RequireReleaseHeader {
				api.WriteError(w, r, http.StatusConflict, "release_outdated",
					rules.ReleaseHeader+" is required; update the game")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// checkRelease classifies r's X-Otomo-Release against the head.
func (s *Server) checkRelease(r *http.Request) string {
	raw := r.Header.Get(rules.ReleaseHeader)
	if raw == "" {
		return releaseMissing
	}
	client, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || client <= 0 {
		return releaseInvalid
	}
	src := s.deps.Release
	if src == nil {
		return releaseUnchecked
	}
	head := src.Head()
	if head == client {
		return releaseCurrent
	}
	head = src.Refresh(r.Context())
	switch {
	case head == 0:
		return releaseUnchecked
	case head == client:
		return releaseCurrent
	default:
		return releaseOutdated
	}
}
