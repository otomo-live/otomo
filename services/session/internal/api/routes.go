package api

import (
	"net/http"

	"github.com/otomo-live/otomo/services/session/internal/auth"
)

// Group names the identity domain a route belongs to, which is what decides which
// verifier its guard uses. It is not derivable from the path: the two prefixes are a
// convention Gateway's route table also follows, but the binding that matters is the
// one in this table, because this is the process a request actually reaches.
type Group int

const (
	GroupPlayer Group = iota + 1 // Auth-issued tokens; `sub` is a player UUID
	GroupStaff                   // PHP Admin Auth-issued staff tokens, with roles
)

// String returns the group's name for logs and readiness messages.
func (g Group) String() string {
	switch g {
	case GroupPlayer:
		return "player"
	case GroupStaff:
		return "staff"
	default:
		return "unknown"
	}
}

// Route is one endpoint this service serves, together with the identity domain that may
// call it and — for staff routes — the least role that may.
//
// The path is the *external* path, prefix and all. Gateway forwards /api/player/session/*
// and /api/admin/session/* without stripping the prefix — the same convention Config's
// /api/admin/config/* routes follow — so a route registered here is the path a client
// actually requests, and the table below can be read against design/04-session-minimal.md
// §5 line for line.
type Route struct {
	Method  string
	Path    string
	Group   Group
	MinRole auth.Role // meaningful only for GroupStaff; player routes carry RoleNone
}

// The two external prefixes. Gateway forwards both without stripping them, so handlers
// are registered under the full path.
const (
	PlayerPrefix = "/api/player/session"
	StaffPrefix  = "/api/admin/session"
)

// Pattern returns the string to register with net/http.ServeMux: "<METHOD> <path>".
func (r Route) Pattern() string {
	return r.Method + " " + r.Path
}

// Routes returns this service's route table: design/04-session-minimal.md §5, in the
// document's order, as the single place where a path, its identity domain and its
// required role are stated.
//
// This branch registers the whole table with 501 placeholders behind it. Doing that now
// rather than as each handler lands is deliberate: the two-issuer boundary is part of
// the service's external contract, it is what SES-A1 and SES-G2 assert, and a route
// that exists but is unimplemented is a much smaller problem than a route that is
// implemented but reachable with the wrong domain's token.
//
// A fresh slice is returned per call so no caller can mutate the table for another.
func Routes() []Route {
	const (
		playerPrefix = PlayerPrefix
		staffPrefix  = StaffPrefix
	)

	return []Route{
		// Profiles. §5 marks PATCH /me as rate-limited (once per 24 h); the limit is a
		// handler property, so the route is registered like any other player route.
		{http.MethodPost, playerPrefix + "/me/init", GroupPlayer, auth.RoleNone},
		{http.MethodGet, playerPrefix + "/me", GroupPlayer, auth.RoleNone},
		{http.MethodPatch, playerPrefix + "/me", GroupPlayer, auth.RoleNone},

		// Presence.
		{http.MethodPost, playerPrefix + "/presence/heartbeat", GroupPlayer, auth.RoleNone},

		// Friends and blocks. Accept and decline are addressed by the requesting
		// player's id rather than by a request row, because a pending friendship is
		// identified by the pair, not by an id of its own (§3.1's composite key).
		{http.MethodGet, playerPrefix + "/friends", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/friends/requests", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/friends/requests/{player_id}/accept", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/friends/requests/{player_id}/decline", GroupPlayer, auth.RoleNone},
		{http.MethodDelete, playerPrefix + "/friends/{player_id}", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/blocks/{player_id}", GroupPlayer, auth.RoleNone},
		{http.MethodDelete, playerPrefix + "/blocks/{player_id}", GroupPlayer, auth.RoleNone},

		// Parties. Kick and promote are leader-only per §5; that is a handler check
		// against the party's leader_id, not a role, because the caller is a player.
		{http.MethodPost, playerPrefix + "/party", GroupPlayer, auth.RoleNone},
		{http.MethodGet, playerPrefix + "/party", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/party/invites", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/party/invites/{invite_id}/accept", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/party/invites/{invite_id}/decline", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/party/leave", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/party/kick/{player_id}", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/party/promote/{player_id}", GroupPlayer, auth.RoleNone},

		// The lobby (design/14-launch-handoff.md §2.1, LB-2). Settings are a leader call
		// and carry the revision; ready is any member's.
		{http.MethodPatch, playerPrefix + "/party/settings", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/party/ready", GroupPlayer, auth.RoleNone},

		// Launch (design/14-launch-handoff.md §2.1, §3, LB-3).
		{http.MethodPost, playerPrefix + "/party/launch", GroupPlayer, auth.RoleNone},
		{http.MethodPost, playerPrefix + "/party/launch/ticket", GroupPlayer, auth.RoleNone},

		// Event long-poll. §4: the handler holds the request up to 25 s, which is why
		// Gateway's read timeout on this path is 35 s.
		{http.MethodGet, playerPrefix + "/events", GroupPlayer, auth.RoleNone},

		// Staff lookup. Read-only, so viewer is the bar.
		{http.MethodGet, staffPrefix + "/players", GroupStaff, auth.RoleViewer},
		{http.MethodGet, staffPrefix + "/players/{id}", GroupStaff, auth.RoleViewer},

		// Force-disband changes other players' state, so it sits above viewer — the
		// same bar Config puts on the acts that alter what players receive. It is
		// audited (SES-A5), which is what makes a higher-than-viewer role meaningful
		// here rather than ceremony.
		{http.MethodPost, staffPrefix + "/parties/{party_id}/disband", GroupStaff, auth.RoleLiveOps},

		// The audit log, read by staff and by Dashboard.
		{http.MethodGet, staffPrefix + "/audit", GroupStaff, auth.RoleViewer},
	}
}
