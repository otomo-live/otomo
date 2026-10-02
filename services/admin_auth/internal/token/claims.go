// The access-token payload.

package token

import "github.com/golang-jwt/jwt/v5"

// Claims is the payload of a staff access token, fixed by the identity contract
// (06 §9.1). Embedding RegisteredClaims is what gives the standard validator and the
// standard field names — iss, sub, aud, exp, nbf, iat — for free.
//
// Roles exists because Gateway parses staff and player tokens into one struct, and
// staff tokens always carry at least one. Name rides along for the audit rows written
// downstream; it is omitted when empty so a nameless token stays compact.
type Claims struct {
	jwt.RegisteredClaims
	Roles []string `json:"roles"`
	Name  string   `json:"name,omitempty"`
}
