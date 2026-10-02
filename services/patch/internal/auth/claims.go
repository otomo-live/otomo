// Package auth verifies the staff tokens this service is called with.
//
// The tokens are issued by PHP Admin Auth and verified here against its JWKS, an
// independent second check behind Gateway's (COM-4, and 00-common-stack.md §1a):
// a private network keeps outsiders out, but every service still re-verifies the
// issuer and audience itself rather than trusting "this came from inside the
// network" as authorization. Rejecting a token is therefore the normal outcome of
// being called directly, and only the second line of defence.
package auth

import (
	"context"

	"github.com/golang-jwt/jwt/v5"
)

// Role is a staff permission level, ordered so that a comparison is the whole of
// the hierarchy check: viewer < live_ops < admin (06-auth-identity-contract.md §5).
type Role int

const (
	RoleViewer  Role = iota + 1
	RoleLiveOps      // may change things on dev and staging
	RoleAdmin        // may change things on live, and roll back
)

// String returns the role's claim spelling, which is also the name it appears under
// in an error message or a log line.
func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleLiveOps:
		return "live_ops"
	case RoleAdmin:
		return "admin"
	default:
		return "none"
	}
}

// ParseRole maps a `roles` claim element onto a Role. The second result is false for
// a name this service does not know, which callers must treat as "grants nothing"
// rather than as an error: a token may legitimately carry a role minted for a later
// version of the hierarchy, and refusing the whole token for it would lock that
// staff member out of everything instead of just the routes that role would have
// granted.
func ParseRole(name string) (Role, bool) {
	switch name {
	case "viewer":
		return RoleViewer, true
	case "live_ops":
		return RoleLiveOps, true
	case "admin":
		return RoleAdmin, true
	default:
		return 0, false
	}
}

// Claims is the verified staff token, as 06-auth-identity-contract.md §5 defines it.
//
// The staff claim set has no name field: `sub` is PHP's own staff account id, and the
// human-readable name lives in PHP's staff table, not in the token. Anything that
// needs to show who did something — the audit log, for instance — resolves the name
// from `sub` downstream.
type Claims struct {
	jwt.RegisteredClaims
	Roles []string `json:"roles,omitempty"`
}

// HasRoleAtLeast reports whether the token carries a role at or above min. A token
// carrying several roles is judged by the highest one present; a token carrying none
// the service recognises fails every min above zero.
func (c *Claims) HasRoleAtLeast(min Role) bool {
	best := Role(0)
	for _, name := range c.Roles {
		if r, ok := ParseRole(name); ok && r > best {
			best = r
		}
	}
	return best >= min
}

// claimsKey is the context key verified claims are stored under. It is an
// unexported struct type, so no importing package can collide with it.
type claimsKey struct{}

// WithClaims returns a copy of ctx carrying c. The server calls this once a request's
// token has been verified, so a handler below can read who is calling without
// re-parsing the Authorization header.
func WithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, c)
}

// ClaimsFrom returns the verified claims stored by WithClaims, or nil when the
// request carries none — which can only happen on a route the auth middleware did not
// guard, and is a bug in the route table rather than a client error.
func ClaimsFrom(ctx context.Context) *Claims {
	c, _ := ctx.Value(claimsKey{}).(*Claims)
	return c
}
