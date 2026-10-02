// Package authn validates access tokens against one JWKS source per
// authentication domain.
//
// Verification is local and always against cached public keys: Gateway never
// calls Auth to ask whether a token is still valid (06-auth-identity-contract.md
// §1). The trade-off that buys is documented in that document's §6 — a revoked
// session stays usable until its access token expires.
package authn

import (
	"context"

	"github.com/golang-jwt/jwt/v5"

	"gateway/gateway/internal/router"
)

// Claims is the token payload both domains emit, per
// 06-auth-identity-contract.md §9.1.
//
// RegisteredClaims carries iss, sub, aud, exp, nbf and iat; aud parses either
// the array form (which both emitters are pinned to produce) or a bare string,
// so nothing downstream has to special-case it.
type Claims struct {
	jwt.RegisteredClaims
	// Roles is present on staff tokens only. Player tokens carry no roles claim
	// at all (contract §4), which is why the player route table's MinRole is 0
	// throughout.
	Roles []string `json:"roles,omitempty"`
}

// roleRank is the staff role hierarchy from the identity contract §5:
// viewer < live_ops < admin. It is ordinal, not a permission bitmask — a token
// carrying several roles is judged by the highest one it holds.
var roleRank = map[string]int{
	"viewer":   1,
	"live_ops": 2,
	"admin":    3,
}

// HasRoleAtLeast reports whether any role on the token meets or exceeds min.
//
// A MinRole of 0 means "any valid token in the group is enough" and is true for
// every token, including one with no roles at all — that is the player domain's
// case, and it is deliberately not a special branch.
func (c *Claims) HasRoleAtLeast(min router.Role) bool {
	best := 0
	for _, r := range c.Roles {
		if rank := roleRank[r]; rank > best {
			best = rank
		}
	}
	return best >= int(min)
}

type claimsKey struct{}

// WithClaims stores the verified claims on the request context, so a downstream
// handler — or an upstream reached through the proxy — never has to re-parse the
// token. Session keys its whole data model on `sub` (contract §4), and this is
// where that value becomes available without a second verification.
func WithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, c)
}

// ClaimsFrom returns the verified claims stored by the middleware, or nil when
// the request did not pass through it (a public route).
func ClaimsFrom(ctx context.Context) *Claims {
	c, _ := ctx.Value(claimsKey{}).(*Claims)
	return c
}
