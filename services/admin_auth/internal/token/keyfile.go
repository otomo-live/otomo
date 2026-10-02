// The on-disk form of the signing key: PKCS#8 PEM, readable only by its owner. The
// private key lives in a file rather than in an environment variable so it can be
// mounted, permissioned and kept out of the process environment.

package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/fs"
	"os"
)

const (
	// pemType is the PEM block label Write emits and Load requires. The generic
	// PKCS#8 label is used because the key is PKCS#8 encoded, not because the type
	// is unknown — Load checks the parsed key's Go type as well.
	pemType = "PRIVATE KEY"

	// privatePerms keeps the key readable only by its owner. It applies at creation,
	// so it also means genkey fails rather than silently writing a world-readable key
	// somewhere it cannot set these bits.
	privatePerms = fs.FileMode(0o600)
)

// Generate returns a freshly generated Ed25519 key pair.
func Generate() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate Ed25519 key: %w", err)
	}
	return pub, priv, nil
}

// Write serialises priv as a PKCS#8 PEM file at path with privatePerms. It does not
// check for an existing file first: refusing to overwrite is genkey's decision, and
// keeping it there means this function has exactly one job.
func Write(path string, priv ed25519.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal PKCS#8 private key: %w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: der})
	if err := os.WriteFile(path, block, privatePerms); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Load reads a PKCS#8 PEM Ed25519 private key.
//
// Every rejection names path and what was actually found — no PEM block, a different
// block label, a different key type — so an operator with several key files can tell
// which one is wrong, and so a wrong-but-valid file fails here rather than as a
// confusing signing error later.
func Load(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block found", path)
	}
	if block.Type != pemType {
		return nil, fmt.Errorf("%s: PEM block is %q, want %q", path, block.Type, pemType)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: parse PKCS#8: %w", path, err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: holds %T, want ed25519.PrivateKey", path, parsed)
	}
	return priv, nil
}

// Public returns the public half of priv. The type assertion cannot fail for a key
// that came from Load or Generate — both produce ed25519.PrivateKey — so a nil result
// would mean a caller built the key some other way.
func Public(priv ed25519.PrivateKey) ed25519.PublicKey {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil
	}
	return pub
}
