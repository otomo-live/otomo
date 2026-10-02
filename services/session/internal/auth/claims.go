// Package auth verifies the tokens this service is called with, from two separate
// identity domains.
//
// Player routes take Auth-issued player tokens; the small admin surface under
// /api/admin/session takes PHP Admin Auth staff tokens. The two are verified against
// different JWKS endpoints, different issuers and different audiences, and a token
// from one domain is refused on the other's routes — that separation is SES-A1's
// acceptance criterion and the reason this package holds one Verifier type built two
// ways rather than one verifier with a mode flag.
//
// Verification here is an independent second check behind Gateway's (COM-4 and
// 00-common-stack.md §1a): a private network keeps outsiders out, but every service
// still verifies issuer and audience itself rather than treating "this came from
// inside the network" as authorization. So rejecting a token is the normal outcome of
// being called directly, and only the second line of defence.
package auth

import (
	"context"

	"github.com/golang-jwt/jwt/v5"
	"uuid"
)

// Domain names the identity domain a Verifier was built for. It appears in log lines
// and readiness messages, where "which domain is not ready" is the first thing an
// operator needs to know.
type Domain string

const (
	DomainPlayer Domain = "player" // Auth-issued tokens, `sub` is a player UUID
	DomainStaff  Domain = "staff"  // PHP Admin Auth-issued tokens, `sub` is a staff account id
)

// Role is a staff permission level, ordered so that a comparison is the whole of the
// hierarchy check: viewer < live_ops < admin (06-auth-identity-contract.md §5).
type Role int

const (
	RoleNone    Role = iota // no role at all; what a route with no minimum asks for
	RoleViewer              // read-only staff access
	RoleLiveOps             // may change things on dev and staging
	RoleAdmin               // may change things on live, and roll back
)

// String returns the role's claim spelling, which is also the name it appears under in
// an error message or a log line.
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

// ParseRole maps a `roles` claim element onto a Role. The second result is false for a
// name this service does not know, which callers must treat as "grants nothing" rather
// than as an error: a token may legitimately carry a role minted for a later version of
// the hierarchy, and refusing the whole token for it would lock that staff member out of
// everything instead of just the routes that role would have granted.
func ParseRole(name string) (Role, bool) {
	switch name {
	case "viewer":
		return RoleViewer, true
	case "live_ops":
		return RoleLiveOps, true
	case "admin":
		return RoleAdmin, true
	default:
		return RoleNone, false
	}
}

// Identity is a verified token after the checks both domains share: signature, issuer,
// audience, expiry, and a usable subject.
//
// One type covers both domains because everything downstream needs the same two
// things — who is calling, and what they may do — and the difference between the
// domains is a property of the Verifier that produced the Identity, not of the result.
// Roles is empty for a player token by construction; see Verifier.Verify.
type Identity struct {
	Subject string   // the `sub` claim: a player UUID, or PHP's staff account id
	Roles   []string // staff only; always nil for a player token
	Name    string   // staff only: the `name` claim PHP Admin Auth adds; may be empty
}

// ActorName is the name an audit row records for this identity: the `name` claim when
// the token carries one, and the subject otherwise, as Config does. A row that says
// "someone did this" is not an audit trail.
func (i *Identity) ActorName() string {
	if i.Name != "" {
		return i.Name
	}
	return i.Subject
}

// HasRoleAtLeast reports whether the identity carries a role at or above min. An
// identity carrying several roles is judged by the highest one present; one carrying no
// role this service recognises fails every min above RoleNone.
func (i *Identity) HasRoleAtLeast(min Role) bool {
	best := RoleNone
	for _, name := range i.Roles {
		if r, ok := ParseRole(name); ok && r > best {
			best = r
		}
	}
	return best >= min
}

// PlayerUUID returns the subject as a parsed UUID.
//
// It cannot fail: the player verifier refuses a token whose `sub` is not a UUID before
// returning an Identity, so by the time a handler holds one the parse has already
// happened. Re-parsing rather than caching the UUID keeps Identity comparable and
// free of a type that a staff subject could not honestly share.
func (i *Identity) PlayerUUID() uuid.UUID {
	id, err := uuid.Parse(i.Subject)
	if err != nil {
		// Unreachable through Verify. A zero UUID here means a caller constructed an
		// Identity by hand, which is a bug in this service rather than a client error,
		// and the queries it feeds would simply match nothing.
		return uuid.UUID{}
	}
	return id
}

// claimsKey is the context key a verified Identity is stored under. It is an
// unexported struct type, so no importing package can collide with it.
type claimsKey struct{}

// WithIdentity returns a copy of ctx carrying id. The server calls this once a
// request's token has been verified, so a handler below can read who is calling without
// re-parsing the Authorization header.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, claimsKey{}, id)
}

// IdentityFrom returns the verified identity stored by WithIdentity, or nil when the
// request carries none — which can only happen on a route whose guard was not applied,
// and is a bug in the route table rather than a client error.
func IdentityFrom(ctx context.Context) *Identity {
	id, _ := ctx.Value(claimsKey{}).(*Identity)
	return id
}

// sessionClaims is the wire shape both domains are parsed into: registered claims plus
// an optional `roles` array. Parsing both domains into one struct is what lets the two
// verifiers share a single gated parse — there is one place where issuer, audience,
// algorithm and expiry are checked, so a change there cannot be applied to one domain
// and forgotten on the other.
type sessionClaims struct {
	jwt.RegisteredClaims
	Roles []string `json:"roles,omitempty"`
	Name  string   `json:"name,omitempty"` // staff display name, for audit rows (SE-7)
}
