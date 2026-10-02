// GET /internal/patch/{channel}/server-manifest (CF-3): the server manifest
// of a channel's current release, for Session's rules loader (LB-1) and later game
// servers.
//
// It is mounted only on the internal listener, behind a D4 service key (see the server
// package); nothing here checks the caller. It answers like the client manifest: an
// ETag from the manifest's SHA-256, Cache-Control: no-cache, and 304 for a matching
// If-None-Match, because LB-1 polls it every 60 s.

package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/otomo-live/otomo/services/patch/internal/manifest"
)

// ServerManifest returns the handler. A release with no stored server manifest serves an
// empty one (the loader builds it), never an error.
func ServerManifest(holder *manifest.Holder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.PathValue("channel")
		if !manifestChannels[channel] {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such channel")
			return
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

		h := w.Header()
		h.Set("ETag", e.Server.ETag)
		h.Set("Cache-Control", "no-cache")
		// The release the manifest belongs to, so a caller can tell which release it
		// holds without parsing the body (SE-8 compares clients against it).
		h.Set("X-Otomo-Release", strconv.FormatInt(e.ReleaseID, 10))

		if ifNoneMatch(r, e.Server.ETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(e.Server.Body)
	}
}
