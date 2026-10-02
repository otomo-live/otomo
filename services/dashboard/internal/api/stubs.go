// The placeholder behind a route in the §4 table whose dependencies are not wired. Every
// route is implemented now (overview by DSH-C5, series by DSH-C6, logs by DSH-C8, tail by
// DSH-C9 and audit by DSH-C11); this is what a nil *Handlers still answers.

package api

import "net/http"

// NotImplemented is the fail-closed handler a route falls back to when the handler seam
// has no implementation for its pattern — a nil *Handlers, or a pattern the table does not
// know. It answers 501 rather than 404 so a client can tell "not built yet" from "wrong
// URL" and fail accordingly.
//
// It is reached only after the auth middleware has admitted the request, so a 501 here
// still means the caller presented a valid staff token with a sufficient role.
func NotImplemented(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotImplemented, "not_implemented", "this endpoint is not implemented yet")
}
