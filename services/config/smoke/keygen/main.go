// Command keygen writes the two inputs an end-to-end run of `serve` needs: a JWKS
// document on disk, to be served by any static file server, and one staff token per
// outcome the smoke test asserts, signed by the matching key.
//
// It exists because this service has no key-material command of its own, and should
// not have one: Config verifies tokens and never mints them, so the staff signing key
// belongs to PHP Admin Auth. Auth's smoke test can generate its keys with the service
// itself; this one needs something that stands in for the issuer, which is what this
// program is. It is not part of the image and nothing else imports it.
//
// The file names it writes are the interface smoke.sh depends on: `jwks.json` under
// `.well-known/`, and `<name>.jwt` per case (`admin`, `live_ops`, `viewer`, `expired`,
// `player`, `forged`).
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	kid      = "smoke-staff-key"
	issuer   = "https://php-admin.otomo.internal"
	audience = "otomo:staff"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: keygen <outdir>")
		os.Exit(2)
	}
	dir := os.Args[1]

	pub, priv, err := ed25519.GenerateKey(nil)
	check(err)

	doc, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "OKP",
		"crv": "Ed25519",
		"kid": kid,
		"x":   base64.RawURLEncoding.EncodeToString(pub),
		"alg": "EdDSA",
		"use": "sig",
	}}})
	check(err)

	wellKnown := filepath.Join(dir, ".well-known")
	check(os.MkdirAll(wellKnown, 0o755))
	check(os.WriteFile(filepath.Join(wellKnown, "jwks.json"), doc, 0o644))

	now := time.Now()
	base := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss": issuer,
			"aud": audience,
			"sub": "42",
			"iat": now.Unix(),
			"exp": now.Add(time.Hour).Unix(),
		}
	}

	// One token per outcome the smoke test asserts.
	tokens := map[string]jwt.MapClaims{
		"admin":    withRoles(base(), "admin"),
		"live_ops": withRoles(base(), "live_ops"),
		"viewer":   withRoles(base(), "viewer"),
		"expired":  withExpiry(base(), now.Add(-time.Hour)),
		"player":   withAudience(base(), "otomo:player"),
	}
	for name, claims := range tokens {
		check(os.WriteFile(filepath.Join(dir, name+".jwt"), []byte(sign(priv, claims)), 0o644))
	}

	// A token claiming the same kid as the published key but signed by a different
	// one: the signature check itself, which is the only thing standing between a
	// valid-looking claim set and an accepted token.
	_, otherPriv, err := ed25519.GenerateKey(nil)
	check(err)
	check(os.WriteFile(filepath.Join(dir, "forged.jwt"),
		[]byte(sign(otherPriv, withRoles(base(), "admin"))), 0o644))

	fmt.Println("wrote jwks.json and", len(tokens)+1, "tokens to", dir)
}

func withRoles(c jwt.MapClaims, roles ...string) jwt.MapClaims {
	c["roles"] = roles
	return c
}

func withAudience(c jwt.MapClaims, aud string) jwt.MapClaims {
	c["aud"] = aud
	return c
}

func withExpiry(c jwt.MapClaims, exp time.Time) jwt.MapClaims {
	c["exp"] = exp.Unix()
	return c
}

func sign(priv ed25519.PrivateKey, claims jwt.MapClaims) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	check(err)
	return s
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		os.Exit(1)
	}
}
