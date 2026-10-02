package api

import (
	"net/http"

	"github.com/otomo-live/otomo/services/dashboard/internal/auth"
)

// Route is one endpoint this service serves, together with the least role that may call
// it.
//
// The path is the *external* path, prefix and all. Gateway forwards
// /api/admin/dashboard/* without stripping the prefix — the same convention Config's
// /api/admin/config/* routes follow — so a route registered here is the path a staff
// member's browser actually requests, and the table below can be read against
// design/01-dashboard.md §4 line for line.
type Route struct {
	Method  string
	Path    string
	MinRole auth.Role
}

// Pattern returns the string to register with net/http.ServeMux: "<METHOD> <path>".
func (r Route) Pattern() string {
	return r.Method + " " + r.Path
}

// Routes returns this service's route table: design/01-dashboard.md §4, in the document's
// order, as the single place where a path and its required role are stated.
//
// Phase C registers the whole table with 501 placeholders behind it. Doing that now rather
// than as each handler lands is deliberate: the role boundaries and the paths are part of
// the service's external contract, they are what the Hurl tests will assert, and a route
// that exists but is unimplemented is a much smaller problem than a route that is
// implemented but accidentally reachable by the wrong role.
//
// Every route here is viewer or higher: the Dashboard is a read-only query surface, so
// there is no live_ops or admin route to state. A fresh slice is returned per call so no
// caller can mutate the table for another.
func Routes() []Route {
	const prefix = "/api/admin/dashboard"

	return []Route{
		{http.MethodGet, prefix + "/overview", auth.RoleViewer},
		{http.MethodGet, prefix + "/services", auth.RoleViewer},
		{http.MethodGet, prefix + "/services/{name}/series", auth.RoleViewer},
		{http.MethodGet, prefix + "/logs", auth.RoleViewer},
		{http.MethodGet, prefix + "/logs/tail", auth.RoleViewer},
		{http.MethodGet, prefix + "/audit", auth.RoleViewer},
	}
}
