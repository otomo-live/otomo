// Command keygen is the smoke test's stand-in for the two identity providers Session
// trusts. It generates one Ed25519 key pair per domain, serves their JWKS documents, and
// mints tokens signed by them.
//
// It is a test fixture, not part of the service. Nothing here is built into the image, and
// Session itself never holds a signing key (see session.go): a deployment mints player
// tokens in Auth and staff tokens in PHP Admin Auth, and Session only ever verifies.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"uuid"
)

const usage = `usage: keygen <command>

commands:
  init <dir>                           generate one Ed25519 key pair per domain
  serve <dir> <addr>                   serve the two JWKS documents over HTTP
  token <dir> <player|staff> <sub> [role...]
                                       print a signed token for that domain

The issuer and audience values match .env.example, so a server started from that file
accepts these tokens without further configuration.`

// domainOf is one identity domain: the key files it uses and the iss/aud a token for it
// must carry. The two differ in every dimension, which is the boundary the smoke test is
// there to check.
type domain struct {
	name     string
	issuer   string
	audience string
}

var domains = map[string]domain{
	"player": {name: "player", issuer: "https://auth.otomo.internal", audience: "otomo:player"},
	"staff":  {name: "staff", issuer: "https://php-admin.otomo.internal", audience: "otomo:staff"},
}

// kid is the key id both domains' JWKS advertise and both tokens carry in their header. It
// is per-domain so a mixed-up key set is visible in the JWKS rather than only in a failed
// verification.
func (d domain) kid() string { return "smoke-" + d.name }

func (d domain) keyPath(dir string) string  { return filepath.Join(dir, d.name+".key") }
func (d domain) jwksPath(dir string) string { return filepath.Join(dir, d.name+".jwks.json") }

func main() {
	log.SetFlags(0)
	if err := run(os.Args[1:]); err != nil {
		log.SetOutput(os.Stderr)
		log.Printf("keygen: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "init":
		if len(args) != 2 {
			return errors.New("usage: keygen init <dir>")
		}
		return initDomain(args[1])
	case "serve":
		if len(args) != 3 {
			return errors.New("usage: keygen serve <dir> <addr>")
		}
		return serve(args[1], args[2])
	case "token":
		if len(args) < 4 {
			return errors.New("usage: keygen token <dir> <player|staff> <sub> [role...]")
		}
		tok, err := mint(args[1], args[2], args[3], args[4:])
		if err != nil {
			return err
		}
		fmt.Println(tok)
		return nil
	default:
		return errors.New(usage)
	}
}

// initDomain writes one key pair and one JWKS document per domain into dir.
func initDomain(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	for _, name := range []string{"player", "staff"} {
		d := domains[name]

		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return fmt.Errorf("generate the %s key: %w", name, err)
		}

		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return fmt.Errorf("marshal the %s key: %w", name, err)
		}
		// The private half is a secret even in a smoke run, and 0600 is what the real
		// issuers use; matching it here keeps the habit.
		if err := os.WriteFile(d.keyPath(dir), pem.EncodeToMemory(&pem.Block{
			Type: "PRIVATE KEY", Bytes: der,
		}), 0o600); err != nil {
			return err
		}

		doc := map[string]any{"keys": []map[string]string{{
			"kty": "OKP",
			"crv": "Ed25519",
			"kid": d.kid(),
			"x":   base64.RawURLEncoding.EncodeToString(pub),
			"alg": "EdDSA",
			"use": "sig",
		}}}
		body, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d.jwksPath(dir), body, 0o644); err != nil {
			return err
		}

		log.Printf("keygen: wrote %s and %s", d.jwksPath(dir), d.keyPath(dir))
	}
	return nil
}

// serve serves dir over HTTP, which is all a JWKS endpoint has to do. It runs until the
// process is killed, so the smoke script starts it in the background.
func serve(dir, addr string) error {
	log.Printf("keygen: serving %s on http://%s", dir, addr)
	return http.ListenAndServe(addr, http.FileServer(http.Dir(dir)))
}

// mint signs a token for one domain. sub is the subject: the player's UUID for the player
// domain (player_profile's primary key, so Session parses it), and an account identifier
// for staff, where no shape is required.
func mint(dir, domainName, sub string, roles []string) (string, error) {
	d, ok := domains[domainName]
	if !ok {
		return "", fmt.Errorf("unknown domain %q; want player or staff", domainName)
	}
	if domainName == "player" {
		if _, err := uuid.Parse(sub); err != nil {
			return "", fmt.Errorf("the player subject must be a UUID, got %q: %w", sub, err)
		}
	}

	raw, err := os.ReadFile(d.keyPath(dir))
	if err != nil {
		return "", fmt.Errorf("read the %s key (run init first): %w", domainName, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", fmt.Errorf("%s is not a PEM file", d.keyPath(dir))
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return "", fmt.Errorf("%s holds a %T, not an Ed25519 key", d.keyPath(dir), parsed)
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"iss": d.issuer,
		"aud": d.audience,
		"sub": sub,
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	if len(roles) > 0 {
		claims["roles"] = roles
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = d.kid()
	return tok.SignedString(priv)
}
