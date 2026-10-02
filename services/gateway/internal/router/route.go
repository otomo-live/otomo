package router

// Group classifies which authentication domain a route belongs to.
type Group int

const (
	GroupPublic Group = iota
	GroupPlayer
	GroupStaff
)

func (g Group) String() string {
	switch g {
	case GroupPublic:
		return "public"
	case GroupPlayer:
		return "player"
	case GroupStaff:
		return "staff"
	default:
		return "unknown"
	}
}

// Role represents staff permission levels in ascending order.
type Role int

const (
	RoleViewer Role = iota + 1
	RoleLiveOps
	RoleAdmin
)

// RoleAtLeast reports whether r meets or exceeds the minimum role.
func (r Role) RoleAtLeast(min Role) bool {
	return r >= min
}

// DefaultMaxBody is the request-body cap for routes that do not set MaxBody.
const DefaultMaxBody = 1 << 20 // 1 MiB

// Route defines a single proxied endpoint.
type Route struct {
	Method         string // "GET", "POST", "*"
	Pattern        string // path portion of the net/http.ServeMux pattern
	Upstream       string // key into the proxy registry, e.g. "session"
	StripPrefix    string // prefix removed before forwarding; "" = forward as-is
	Group          Group  // which authentication domain this route belongs to
	MinRole        Role   // 0 = no role check (any valid token in the group suffices)
	Stream         bool   // true = SSE/long-poll: disable buffering, extend deadlines
	SetForwarded   bool   // true = replace X-Forwarded-For with the peer IP and set X-Forwarded-Proto
	MaxBody        int64  // request body cap in bytes; 0 = DefaultMaxBody
	Upload         bool   // true = long request body: clear read and write deadlines
	ForwardCookies bool   // true = pass Cookie upstream; every other route drops it
}

// UsedGroups returns the set of authentication domains the given routes
// reference.
//
// Gateway builds one JWKS client per domain in this set and no others: an
// instance must never fetch a key source its own route table cannot use
// (techspec §6.2). It is also what decides which domains /readyz waits on, so
// adding a route in a new domain extends readiness automatically rather than
// needing a second list kept in sync by hand.
func UsedGroups(routes []Route) map[Group]bool {
	used := make(map[Group]bool)
	for _, r := range routes {
		if r.Group != GroupPublic {
			used[r.Group] = true
		}
	}
	return used
}

// MuxPattern returns the string to register with net/http.ServeMux.
//
// §5.1's comment describes Pattern as a full ServeMux pattern including the
// method (e.g. "GET /api/player/session/events"), but §5.2's Go literals keep
// Method and Pattern as separate fields. This follows the §5.2 literals: when
// Method is "*", register the bare path so ServeMux matches all HTTP methods;
// otherwise register "<METHOD> <path>".
func (r Route) MuxPattern() string {
	if r.Method == "*" {
		return r.Pattern
	}
	return r.Method + " " + r.Pattern
}
