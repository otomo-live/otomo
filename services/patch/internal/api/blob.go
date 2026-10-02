package api

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/otomo-live/otomo/services/patch/internal/blob"
)

// blobSHA256Pattern is the only shape the {sha256} path segment may take. It is the
// path-traversal guard, not a tidiness check: the value becomes a path component, so
// a slash, a ".." or an uppercase spelling is rejected before any filesystem call.
// Config names blobs with exactly this shape, so anything else cannot be one.
var blobSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// blobRoutePrefix is the public prefix the blob route lives under. BlobPrefix
// needs it to recognise a request that ServeMux would otherwise clean and redirect
// before the handler ever runs.
const blobRoutePrefix = "/patch/v1/blob/"

// BlobObserver records the outcome of a blob request by the status that was written.
// The server's metrics registry implements it; the interface keeps this package from
// importing the server and lets a test substitute a counter of its own.
type BlobObserver interface {
	BlobRequest(result string)
}

// Blob serves one content-addressed blob: GET and HEAD, public.
//
// The sha256 is validated against blobSHA256Pattern before the store is touched, so
// no request can name a path outside the configured root. The file is opened
// read-only and handed to http.ServeContent, which streams it and owns the Range /
// If-Range / 206 / 416 and If-None-Match / 304 machinery; this handler only sets the
// caching headers. Nothing is buffered.
//
// Integrity is the fetching client's job: it hashes the bytes it downloaded and
// compares them with the name it requested. Patch does not re-hash on every request —
// that would turn a cheap byte copy into a CPU-bound scan of the whole volume. The
// digest is already the name, and the writer only ever commits complete files.
func Blob(root *blob.Root, obs BlobObserver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusWriter{ResponseWriter: w}
		defer func() {
			if obs != nil {
				if result, ok := blobResult(rec.status); ok {
					obs.BlobRequest(result)
				}
			}
		}()

		sha := r.PathValue("sha256")
		if !blobSHA256Pattern.MatchString(sha) {
			WriteError(rec, r, http.StatusBadRequest, "validation_failed",
				"sha256 must match ^[0-9a-f]{64}$")
			return
		}

		f, info, err := root.Open(sha)
		if err != nil {
			// A missing file, a directory at the name, or any other open/stat
			// failure is the same thing to a client: there is no blob there.
			WriteError(rec, r, http.StatusNotFound, "not_found", "no such blob")
			return
		}
		defer f.Close()

		h := rec.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		// The ETag is strong and set before ServeContent so its If-None-Match
		// check (which reads this header) can answer a revalidation with 304.
		h.Set("ETag", `"`+sha+`"`)

		http.ServeContent(rec, r, "", info.ModTime(), f)
	}
}

// BlobPrefix routes the public blob prefix. A request under it must name a single
// 64-character lowercase hex digest; that check runs here, before the mux, because
// ServeMux cleans a path containing a literal ".." (or a doubled slash) and answers
// with a 307 redirect before any route can match — so without this a traversal
// attempt such as /patch/v1/blob/../../etc/passwd would surface as a redirect to the
// cleaned path instead of the route's own validation.
//
// A well-formed request is handed to blobs, a mux carrying only
// "GET /patch/v1/blob/{sha256}". Keeping that route on its own mux matters: on the
// main mux it would conflict with "GET /patch/v1/{channel}/manifest" (both match
// /patch/v1/blob/manifest) and ServeMux would panic at registration. Everything not
// under the prefix is passed to next untouched.
func BlobPrefix(obs BlobObserver, blobs, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, blobRoutePrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if !blobSHA256Pattern.MatchString(strings.TrimPrefix(r.URL.Path, blobRoutePrefix)) {
			if obs != nil {
				obs.BlobRequest("400")
			}
			WriteError(w, r, http.StatusBadRequest, "validation_failed",
				"sha256 must match ^[0-9a-f]{64}$")
			return
		}
		blobs.ServeHTTP(w, r)
	})
}

// statusWriter minimally captures the status the handler wrote — the blob metric is
// keyed on the actual result, whether it came from ServeContent, WriteError or a
// future code path — while staying a drop-in ResponseWriter. Unwrap exposes the
// underlying writer to http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// blobResult renders the metric label for the five results the
// patch_blob_requests_total series documents. A 416 (or any other status a future
// ServeContent path might write) is deliberately not counted, so the label's
// cardinality stays exactly the set an operator can reason about.
func blobResult(status int) (string, bool) {
	switch status {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified,
		http.StatusNotFound, http.StatusBadRequest:
		return strconv.Itoa(status), true
	default:
		return "", false
	}
}
