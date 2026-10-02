// The placeholder behind every route in the §5 table. Each is replaced wholesale as its
// phase's task lands: profiles by SES-B1/B2, presence by SES-B3…B6, friends and blocks
// by SES-C1…C5, parties by SES-D1…D6, the long-poll by SES-E1, and staff lookup and
// force-disband by the admin handlers.

package api

import "net/http"

// NotImplemented is the handler for a route that exists in the route table but has no
// logic behind it yet. It answers 501 rather than 404 so a client can tell "not built
// yet" from "wrong URL" and fail accordingly.
//
// It is reached only after the guard has admitted the request, so a 501 here still
// means the caller presented a valid token from the route's own identity domain — which
// is what lets the SES-A1 acceptance checks assert the 401 boundaries now, before any
// handler exists: a staff token on a player route is refused by the guard, not by the
// stub.
func NotImplemented(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotImplemented, "not_implemented", "this endpoint is not implemented yet")
}
