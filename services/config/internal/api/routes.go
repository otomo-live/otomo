package api

import (
	"net/http"

	"github.com/otomo-live/otomo/services/config/internal/auth"
)

// Route is one endpoint this service serves, together with the least role that may
// call it.
//
// The path is the *external* path, prefix and all. Gateway forwards /api/admin/config/*
// without stripping the prefix — the same convention Auth's /auth/* routes follow —
// so a route registered here is the path a staff member's browser actually requests,
// and the table below can be read against design/02-config.md §5 line for line.
type Route struct {
	Method  string
	Path    string
	MinRole auth.Role
}

// Pattern returns the string to register with net/http.ServeMux: "<METHOD> <path>".
func (r Route) Pattern() string {
	return r.Method + " " + r.Path
}

// Routes returns this service's route table: design/02-config.md §5, in the document's
// order, as the single place where a path and its required role are stated.
//
// Phase A registers the whole table with 501 placeholders behind it. Doing that now
// rather than as each handler lands is deliberate: the role boundaries are part of the
// service's external contract, they are what CFG-E3's Hurl tests assert, and a route
// that exists but is unimplemented is a much smaller problem than a route that is
// implemented but accidentally reachable by the wrong role.
//
// A fresh slice is returned per call so no caller can mutate the table for another.
func Routes() []Route {
	const prefix = "/api/admin/config"

	return []Route{
		// Namespaces.
		{http.MethodGet, prefix + "/namespaces", auth.RoleViewer},
		{http.MethodPost, prefix + "/namespaces", auth.RoleAdmin},

		// Schema: replacing one creates a new schema_version, so it is an admin act.
		{http.MethodGet, prefix + "/namespaces/{ns}/schema", auth.RoleViewer},
		{http.MethodPut, prefix + "/namespaces/{ns}/schema", auth.RoleAdmin},

		// Drafts. Reading is open to viewers; saving is a live_ops act.
		{http.MethodGet, prefix + "/namespaces/{ns}/draft", auth.RoleViewer},
		{http.MethodPut, prefix + "/namespaces/{ns}/draft", auth.RoleLiveOps},
		{http.MethodPost, prefix + "/namespaces/{ns}/draft/validate", auth.RoleLiveOps},

		// Versions and diff.
		{http.MethodPost, prefix + "/namespaces/{ns}/versions", auth.RoleLiveOps},
		{http.MethodGet, prefix + "/namespaces/{ns}/versions", auth.RoleViewer},
		{http.MethodGet, prefix + "/namespaces/{ns}/versions/{v}", auth.RoleViewer},
		{http.MethodGet, prefix + "/namespaces/{ns}/diff", auth.RoleViewer},

		// Content packs.
		{http.MethodPost, prefix + "/packs", auth.RoleLiveOps},
		{http.MethodGet, prefix + "/packs", auth.RoleViewer},

		// Releases. §5 gives publishing to `live` a higher bar than publishing to
		// `dev` or `staging`, and the table expresses that structurally rather than
		// in a handler: the literal path below is more specific than the wildcard
		// one, so ServeMux resolves a live publish against RoleAdmin. Gateway's
		// admin table is written the same way, with the same two entries.
		{http.MethodPost, prefix + "/channels/live/releases", auth.RoleAdmin},
		{http.MethodPost, prefix + "/channels/{ch}/releases", auth.RoleLiveOps},
		{http.MethodGet, prefix + "/channels/{ch}/releases", auth.RoleViewer},

		// Rollback and promote change what live players receive, so both are admin.
		{http.MethodPost, prefix + "/channels/{ch}/rollback", auth.RoleAdmin},
		{http.MethodPost, prefix + "/channels/{ch}/promote", auth.RoleAdmin},

		// The audit log, read by staff and by Dashboard.
		{http.MethodGet, prefix + "/audit", auth.RoleViewer},
	}
}
