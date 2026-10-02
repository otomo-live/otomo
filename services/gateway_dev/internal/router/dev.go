package router

// BuildRoutes returns the route table for the admin and dev gateway
// (techspec §5.2, admin half).
//
// This is the only table, and there is no flag that selects another. The player
// routes live in services/gateway, which is a separate binary, so no environment
// variable on this container can widen what it serves. Every protected route here
// is GroupStaff: verified against admin-auth's JWKS and admin-auth's issuer and
// audience, never against the player Auth service.
//
// GroupPlayer still exists as a constant and no route uses it. That is
// deliberate rather than leftover: middleware_test.go's cross-domain cases
// (TestPlayerTokenOnStaffRouteIsRejected, TestSharedKeyCrossDomainIsRejectedOnIssuer)
// build their own two-domain registries to prove a player token cannot be
// accepted here, and they need the constant to write that claim down.
//
// Role bars are the coarse edge check; every upstream re-checks per route.
// /api/admin/config/ is gated at viewer because Config's own table is the finer
// one (reads are viewer, writes live_ops or admin), and a stricter edge would
// make every read 403 for a viewer before Config ever saw it. The one Config
// route the edge does hold higher is the live publish, which is the boundary
// between rehearsal and what players receive.
func BuildRoutes() []Route {
	return []Route{
		// admin-auth rate-limits and logs per client, so it must not trust a
		// client-built X-Forwarded-For chain.
		{Method: "*", Pattern: "/admin-auth/", Upstream: "adminauth", Group: GroupPublic, SetForwarded: true, ForwardCookies: true},
		{Method: "*", Pattern: "/admin/", Upstream: "adminui", Group: GroupPublic},
		{Method: "*", Pattern: "/api/admin/config/", Upstream: "config", Group: GroupStaff, MinRole: RoleViewer},
		{Method: "POST", Pattern: "/api/admin/config/channels/live/releases", Upstream: "config", Group: GroupStaff, MinRole: RoleAdmin},
		// Content-pack upload: more specific than the /api/admin/config/ prefix,
		// so ServeMux resolves it first. live_ops matches Config's own bar for a
		// pack upload. Upload clears both deadlines because 512 MiB cannot fit
		// in the server-wide read or write timeout.
		{Method: "POST", Pattern: "/api/admin/config/packs", Upstream: "config", Group: GroupStaff, MinRole: RoleLiveOps, MaxBody: 512 << 20, Upload: true},
		{Method: "*", Pattern: "/api/admin/dashboard/logs/tail", Upstream: "dashboard", Group: GroupStaff, MinRole: RoleViewer, Stream: true},
		{Method: "*", Pattern: "/api/admin/dashboard/", Upstream: "dashboard", Group: GroupStaff, MinRole: RoleViewer},
		{Method: "*", Pattern: "/api/admin/session/", Upstream: "session", Group: GroupStaff, MinRole: RoleViewer},
		// Staff account management, served by admin-auth. The collection root is
		// registered as well as the subtree. With the subtree alone, ServeMux
		// answers /api/admin/users itself with a 307 to /api/admin/users/, and it
		// does so before any auth runs: an anonymous caller would get a redirect
		// confirming the route exists instead of a 401, and every real call to
		// the collection would pay an extra round trip. Like /admin-auth/, these
		// set SetForwarded because admin-auth rate-limits per client.
		{Method: "*", Pattern: "/api/admin/users", Upstream: "adminauth", Group: GroupStaff, MinRole: RoleAdmin, SetForwarded: true, ForwardCookies: true},
		{Method: "*", Pattern: "/api/admin/users/", Upstream: "adminauth", Group: GroupStaff, MinRole: RoleAdmin, SetForwarded: true, ForwardCookies: true},
	}
}
