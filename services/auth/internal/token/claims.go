// The access-token payload.

package token

import "github.com/golang-jwt/jwt/v5"

// Claims is the payload of an access token, fixed by the identity contract
// (06 §9.1). Embedding RegisteredClaims is what gives the standard validator and the
// standard field names — iss, sub, aud, exp, nbf, iat — for free.
//
// Roles exists because Gateway parses staff and player tokens into one struct; Auth
// issues player tokens only, so it is always empty here. The field is kept so both
// sides of the contract stay structurally identical.
type Claims struct {
	jwt.RegisteredClaims
	Roles []string `json:"roles,omitempty"`
}
