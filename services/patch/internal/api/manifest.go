package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/otomo-live/otomo/services/patch/internal/auth"
	"github.com/otomo-live/otomo/services/patch/internal/manifest"
)

// manifestChannels is the closed set of channels the manifest route knows. Anything
// else is a COM-5 404, not a 403: a caller should not be able to tell an unknown
// channel from one it may not see on the live route, and the set is small enough to
// keep as a map.
var manifestChannels = map[string]bool{
	"dev":     true,
	"staging": true,
	"live":    true,
}

// ManifestObserver records the outcome of a manifest request. The server's metrics
// registry implements it; the interface keeps this package from importing the server
// and lets a test substitute a counter of its own.
type ManifestObserver interface {
	// ManifestRequest counts one served manifest request, by channel and result
	// ("200" or "304"). It is deliberately not called for 4xx/5xx: the metric
	// documents what clients actually received as a manifest.
	ManifestRequest(channel, result string)
	// TokenRejected counts a dev/staging rejection by the same reason code that
	// appears in the response body and in the access log.
	TokenRejected(reason string)
}

// Manifest is the public manifest handler. It serves one channel's current release
// out of an immutable in-memory Set: the only per-request work is one Holder.Load,
// one map lookup and one header compare, so a manifest check is cheap enough to take
// on every poll.
//
// live is public. dev and staging re-verify a staff bearer token here even though the
// player gateway already gates those routes — defence in depth, exactly as the rest of
// the stack re-verifies rather than trusting the private network. The verifier's own
// reason code is the COM-5 code on a 401, so the response, the metric and the log all
// name the same cause.
//
// holder may be nil (an unready server) and verifier may be nil (no JWKS configured):
// both fail closed rather than panicking, which is what keeps a route registered before
// its dependencies are wired from turning a request into a 500.
func Manifest(holder *manifest.Holder, verifier *auth.Verifier, obs ManifestObserver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.PathValue("channel")
		if !manifestChannels[channel] {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such channel")
			return
		}

		if channel != "live" {
			if !authorize(w, r, verifier, obs) {
				return
			}
		}

		var set *manifest.Set
		if holder != nil {
			set = holder.Load()
		}
		e, ok := set.Get(channel)
		if !ok {
			WriteError(w, r, http.StatusServiceUnavailable, "not_ready",
				fmt.Sprintf("manifest for %s is not loaded", channel))
			return
		}

		// The caching headers go on both 200 and 304: a client that gets a 304 needs
		// the same validators to send on its next check, and X-Min-Client-Version has
		// to reach a client that is already up to date too — that is exactly the
		// client deciding whether it may still run.
		h := w.Header()
		h.Set("ETag", e.ETag)
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Min-Client-Version", e.MinClientVersion)

		if ifNoneMatch(r, e.ETag) {
			if obs != nil {
				obs.ManifestRequest(channel, "304")
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}

		if obs != nil {
			obs.ManifestRequest(channel, "200")
		}
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// A HEAD response carries the same headers but no body. The body still counts
		// as a served manifest, so the metric above records 200 either way.
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(e.Body)
	}
}

// authorize checks a dev or staging request's bearer token. It reports whether the
// request may proceed and, when it may not, writes the COM-5 error and records the
// rejection. A valid token without a recognised role is a 403, not a 401: the caller
// proved who it is and simply may not see this channel.
func authorize(w http.ResponseWriter, r *http.Request, verifier *auth.Verifier, obs ManifestObserver) bool {
	var (
		claims *auth.Claims
		reason string
	)
	if verifier == nil {
		reason = auth.ReasonInvalidToken
	} else {
		claims, reason = verifier.Verify(r.Context(), auth.BearerToken(r))
	}
	if reason != "" {
		reject(w, r, obs, http.StatusUnauthorized, reason)
		return false
	}
	if !claims.HasRoleAtLeast(auth.RoleViewer) {
		reject(w, r, obs, http.StatusForbidden, auth.ReasonInsufficientRole)
		return false
	}
	return true
}

func reject(w http.ResponseWriter, r *http.Request, obs ManifestObserver, status int, reason string) {
	if obs != nil {
		obs.TokenRejected(reason)
	}
	WriteError(w, r, status, reason, auth.MessageFor(reason))
}

// ifNoneMatch reports whether the request's If-None-Match header selects etag. It
// implements RFC 9110 §13.1.2: "*" matches any current representation, otherwise the
// header is a comma-separated list of entity-tags compared weakly — a W/ prefix on
// either the header member or the current tag is ignored and the quoted opaque-tags
// are compared. Whitespace around members is tolerated and a member that is not a
// valid entity-tag is skipped rather than failing the whole request.
func ifNoneMatch(r *http.Request, etag string) bool {
	header := strings.Join(r.Header.Values("If-None-Match"), ",")
	if strings.TrimSpace(header) == "" {
		return false
	}
	current, ok := opaqueTag(etag)
	if !ok {
		return false
	}
	for _, member := range strings.Split(header, ",") {
		member = strings.TrimSpace(member)
		if member == "*" {
			return true
		}
		if tag, ok := opaqueTag(member); ok && tag == current {
			return true
		}
	}
	return false
}

// opaqueTag parses one entity-tag and returns its opaque-tag (the bytes between the
// quotes). The weak indicator W/ is ignored, which is the only difference between
// weak and strong comparison. The second result is false for anything that is not a
// well-formed entity-tag, including an empty string and a tag with control characters
// or an inner quote.
func opaqueTag(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", false
	}
	body := s[1 : len(s)-1]
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c < 0x21 || c > 0x7e {
			return "", false
		}
	}
	return body, true
}
