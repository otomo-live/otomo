// Package servicekey reads the shared secret the proxy presents to the Allocator.
//
// The proxy has no HTTP API of its own, so unlike the Allocator it does not
// authenticate callers: the only key it needs is the one it spends. It is a base64url
// file rather than an environment value so it can be mounted read-only and kept out of
// the process environment, and it is validated at start-up so a missing or malformed
// secret fails the container instead of the first directory poll.
//
// Adapted from services/allocator/internal/servicekey/servicekey.go, which owns the
// matching server side: the Allocator accepts the key this package loads. Keep the
// decode and minimum-length rules in step with that file.

package servicekey

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// minKeyBytes is the shortest decoded secret ReadKeyFile accepts. 32 bytes is the size
// of a SHA-256 digest and of a modern symmetric key; anything shorter is a sign that a
// key was typed by hand rather than generated.
const minKeyBytes = 32

// decodeKey turns a key file's text into its secret. Both padded and unpadded
// base64url are accepted, because an operator who generated a key with the URL-safe
// alphabet may or may not have kept the padding; the decoded bytes are the key either
// way.
func decodeKey(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimRight(text, "=")
	return base64.RawURLEncoding.DecodeString(text)
}

// ReadKeyFile reads path and returns its trimmed base64url text, ready to be sent as a
// bearer token.
//
// The file is validated — base64url and at least minKeyBytes decoded — but the
// returned value is the file's own spelling, because the Allocator decodes the token
// before comparing and only the bytes have to match. Key material is never included in
// an error; the messages name the path and what was wrong with it, and the contents
// stop at this function.
func ReadKeyFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read key file %q: %w", path, err)
	}
	text := strings.TrimSpace(string(raw))
	key, err := decodeKey(text)
	if err != nil {
		return "", fmt.Errorf("key file %q is not base64url", path)
	}
	if len(key) < minKeyBytes {
		return "", fmt.Errorf("key file %q holds %d bytes, want at least %d", path, len(key), minKeyBytes)
	}
	return text, nil
}
