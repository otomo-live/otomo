// The placeholder behind every route in the §5 table. Each is replaced wholesale as
// its Phase B task lands: namespaces by CFG-B1, schemas by CFG-B2, drafts by CFG-B3,
// validation by CFG-B4, versions by CFG-B5, diff by CFG-B6, packs by CFG-B7, and
// releases, rollback, promote and audit by CFG-B8/B9.

package api

import "net/http"

// NotImplemented is the handler for a route that exists in the route table but has no
// logic behind it yet. It answers 501 rather than 404 so a client can tell "not built
// yet" from "wrong URL" and fail accordingly.
//
// It is reached only after the auth middleware has admitted the request, so a 501 here
// still means the caller presented a valid staff token with a sufficient role — which
// is what lets the CFG-A1 acceptance checks assert the 401 and 403 boundaries now,
// before any handler exists.
func NotImplemented(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotImplemented, "not_implemented", "this endpoint is not implemented yet")
}
