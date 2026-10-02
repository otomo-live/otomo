// keyserver/main.go: ephemeral signing keys, the JWKS documents the smoke test
// needs, and the tokens signed with them.
//
// Nothing is downloaded and nothing is committed: both keypairs are generated at
// startup, published over HTTP exactly as services/auth and admin-auth
// publish theirs, and discarded when the process exits. That is what lets
// smoke.sh exercise the real middleware with no Docker, no database and no other
// service running.
//
// This service authenticates against the staff document only. The player
// document is served anyway, so the smoke test can prove the point this service
// exists for: a token minted by the player domain is rejected here.
//
// Usage (normally from smoke.sh, which builds and starts it):
//
//	go run ./testdata/smoke/keyserver -addr 127.0.0.1:18092 -tokens /tmp/tokens.env
//
// It serves:
//
//	GET /.well-known/player-jwks.json   the player domain (services/auth)
//	GET /.well-known/staff-jwks.json    the staff domain (admin-auth)
//	GET /healthz
//
// and writes the five tokens the smoke test presents as shell assignments to
// -tokens, then keeps serving until it is killed.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	// The service's own claim shape, imported rather than re-declared: a token
	// this helper mints has to be exactly the payload the middleware is written
	// against, and sharing the type is the only way to be sure of that. The
	// import is legal because this package lives inside the module rooted at the
	// parent of internal/.
	"github.com/otomo-live/otomo/services/gateway_dev/internal/authn"
)

// The two identities, character for character what the services and
// internal/authn/middleware_test.go use.
const (
	playerIssuer   = "https://auth.otomo.internal"
	playerAudience = "otomo:player"
	staffIssuer    = "https://admin-auth.otomo.internal"
	staffAudience  = "otomo:staff"
)

// Distinct kid per domain, matching the two real emitters. This is load-bearing
// for the smoke test, not decoration: because the two domains publish different
// kids, a player token on a staff route dies at key selection and is reported as
// invalid_signature rather than iss_mismatch. See the note on reasonFor in
// internal/authn/middleware.go.
const (
	playerKID = "player-smoke-1"
	staffKID  = "staff-smoke-1"
)

// Subjects tagged SMOKE, so smoke.sh check 19 can grep /metrics for them and
// find nothing. An identity must never reach a metric label.
const (
	playerSubject = "player-uuid-SMOKE-9012345"
	staffSubject  = "staff-uuid-SMOKE-6789012"
)

// tokenTTL is far longer than a smoke run, so no check is ever racing expiry.
const tokenTTL = 30 * time.Minute

// keyPair is one Ed25519 keypair and the kid it is published under.
type keyPair struct {
	kid  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newKeyPair(kid string) keyPair {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		log.Fatalf("generating %s: %v", kid, err)
	}
	return keyPair{kid: kid, pub: pub, priv: priv}
}

// jwks renders the public half as the JWK Set a JWKS endpoint serves. The shape
// is the one internal/authn's test fixture publishes and the one keyfunc
// expects: OKP / Ed25519 / use sig / alg EdDSA.
func (k keyPair) jwks() []byte {
	out, err := json.Marshal(map[string]any{
		"keys": []map[string]any{{
			"kty": "OKP",
			"crv": "Ed25519",
			"kid": k.kid,
			"x":   base64.RawURLEncoding.EncodeToString(k.pub),
			"use": "sig",
			"alg": "EdDSA",
		}},
	})
	if err != nil {
		log.Fatalf("rendering the %s jwks: %v", k.kid, err)
	}
	return out
}

// sign mints a token with kid in the header, so the verifier selects the key the
// way it would in production.
func (k keyPair) sign(claims authn.Claims) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = k.kid
	s, err := tok.SignedString(k.priv)
	if err != nil {
		log.Fatalf("signing: %v", err)
	}
	return s
}

// claims builds the payload both emitters produce: registered claims plus, for
// staff, the roles array. A nil roles slice is omitted entirely by the
// `omitempty` on authn.Claims.Roles, which is how a player token (contract §4)
// and the no-roles staff token are both produced here.
func claims(issuer, audience, subject string, roles []string) authn.Claims {
	now := time.Now()
	return authn.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{audience},
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
		},
		Roles: roles,
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18092", "address to serve the JWKS documents on")
	tokens := flag.String("tokens", "", "path to write the token shell assignments to")
	flag.Parse()

	if *tokens == "" {
		log.Fatal("-tokens is required")
	}

	player := newKeyPair(playerKID)
	staff := newKeyPair(staffKID)

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/player-jwks.json", serveJWKS(player))
	mux.HandleFunc("/.well-known/staff-jwks.json", serveJWKS(staff))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Bind first, then announce. Writing the tokens file is the signal smoke.sh
	// waits on, so it must not happen for a keyserver that failed to get its
	// port. Otherwise the script races ahead against a process that is already
	// dead.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("keyserver cannot listen on %s: %v", *addr, err)
	}

	// Single-quoted shell assignments. A JWT is base64url plus dots, so it holds
	// no character the shell would interpret.
	body := fmt.Sprintf(
		"PLAYER_TOKEN='%s'\n"+
			"STAFF_NO_ROLES_TOKEN='%s'\n"+
			"STAFF_VIEWER_TOKEN='%s'\n"+
			"STAFF_LIVEOPS_TOKEN='%s'\n"+
			"STAFF_ADMIN_TOKEN='%s'\n",
		player.sign(claims(playerIssuer, playerAudience, playerSubject, nil)),
		staff.sign(claims(staffIssuer, staffAudience, staffSubject, nil)),
		staff.sign(claims(staffIssuer, staffAudience, staffSubject, []string{"viewer"})),
		staff.sign(claims(staffIssuer, staffAudience, staffSubject, []string{"live_ops"})),
		staff.sign(claims(staffIssuer, staffAudience, staffSubject, []string{"admin"})),
	)
	if err := os.WriteFile(*tokens, []byte(body), 0o600); err != nil {
		log.Fatalf("writing %s: %v", *tokens, err)
	}

	log.Printf("keyserver listening on %s (kids %s, %s); tokens written to %s",
		*addr, playerKID, staffKID, *tokens)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(ln); err != nil {
		log.Fatalf("keyserver: %v", err)
	}
}

func serveJWKS(k keyPair) http.HandlerFunc {
	doc := k.jwks()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}
}
