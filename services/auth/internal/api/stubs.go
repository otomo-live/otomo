// Placeholder for a route that is in the table but has no handler wired: what the
// /auth/* routes answer on a server built without a store or signer.

package api

import "net/http"

// NotImplemented is the handler for a route that exists in the route table but has no
// logic behind it yet. It answers 501 rather than 404 so a client can tell "not built
// yet" from "wrong URL" and fail accordingly.
func NotImplemented(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotImplemented, "not_implemented", "this endpoint is not implemented yet")
}
