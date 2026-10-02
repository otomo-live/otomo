// The join-ticket payload.

package token

import "github.com/golang-jwt/jwt/v5"

// Claims is the payload of a join ticket, fixed by the launch hand-off contract
// (doc 14 §5). Embedding RegisteredClaims gives the standard validator and the standard
// field names — iss, sub, aud, exp, iat, jti — for free.
//
// Alloc and Srv tie the ticket to one allocation on one game server, so the proxy and
// the game server can refuse a ticket minted for a different match without consulting
// the allocator.
type Claims struct {
	jwt.RegisteredClaims
	Alloc string `json:"alloc"`
	Srv   string `json:"srv"`
}
