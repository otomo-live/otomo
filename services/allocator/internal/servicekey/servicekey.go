// Package servicekey authenticates the Allocator's internal callers with shared
// secrets (design decision D4).
//
// Every caller — Session asking for a game server, a game server registering or
// heartbeating, the Gameplay Proxy reading the pool, and the Allocator itself when it
// calls Session back — presents a key as an Authorization: Bearer token. Each key lives
// in its own file so a leak of one shared secret does not force a rotation of all of
// them, and a key is valid for one role only: Session's key cannot register a game
// server, and a game server's key cannot ask for one.
//
// The keys are long-lived shared secrets, not tokens, so they carry no expiry and no
// claims. What they prove is "a component that was given this secret", which is all an
// internal Docker network needs; the game ticket the Allocator later mints is what
// binds a player to a server.
package servicekey

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/otomo-live/otomo/services/allocator/internal/apiresp"
)

// Role names a caller and the routes it may reach.
type Role string

const (
	// RoleSession is the Session service asking for a game server for a party.
	RoleSession Role = "session"
	// RoleGameServer is any game server registering, heartbeating or reporting a
	// result.
	RoleGameServer Role = "gameserver"
	// RoleProxy is the Gameplay Proxy looking up where the server named by a join
	// ticket's srv claim lives, so it can forward the player's UDP traffic there.
	RoleProxy Role = "proxy"
)

// minKeyBytes is the shortest decoded secret LoadKeys accepts. 32 bytes is the size
// of a SHA-256 digest and of a modern symmetric key; anything shorter is a sign that a
// key was typed by hand rather than generated.
const minKeyBytes = 32

// roleKey is the context key a verified role is stored under. It is an unexported
// struct type, so no importing package can collide with it.
type roleKey struct{}

// RoleFrom returns the role Require stored on the context, or false when the request
// did not come through Require.
func RoleFrom(ctx context.Context) (Role, bool) {
	role, ok := ctx.Value(roleKey{}).(Role)
	return role, ok
}

// Keys is the set of service keys, one per role. Build it once at start-up with
// LoadKeys; it is read-only afterwards and safe for concurrent use.
type Keys struct {
	byRole map[Role][]byte
	// roles is the fixed order Require scans in. A slice rather than ranging over the
	// map so every request compares against the keys in the same order, which keeps
	// the per-role timing from depending on Go's map iteration.
	roles []Role
}

// decodeKey turns a key file's text into its secret. Both padded and unpadded
// base64url are accepted, because an operator who generated a key with the URL-safe
// alphabet may or may not have kept the padding; the decoded bytes are the key either
// way.
func decodeKey(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimRight(text, "=")
	return base64.RawURLEncoding.DecodeString(text)
}

// LoadKeys reads one key per role from paths and returns them as a set.
//
// Every problem is collected before returning, so one start-up reports a missing file,
// a bad encoding and a too-short key together. Key material is never included in an
// error: the messages name the role and the file path, and the contents of the file
// stop at this function.
//
// Two roles sharing a key is rejected rather than silently allowed: if Session and a
// game server present the same secret, the role split has stopped meaning anything and
// a leak of that one secret compromises both directions.
func LoadKeys(paths map[Role]string) (*Keys, error) {
	// Sort first so the error message and the per-role timing do not depend on map
	// iteration order.
	roles := make([]Role, 0, len(paths))
	for role := range paths {
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })

	var problems []string
	keys := &Keys{byRole: make(map[Role][]byte, len(roles))}
	seen := make(map[string]Role, len(roles))

	for _, role := range roles {
		path := paths[role]
		raw, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("read %s key file %q: %v", role, path, err))
			continue
		}
		key, err := decodeKey(string(raw))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s key file %q is not base64url", role, path))
			continue
		}
		if len(key) < minKeyBytes {
			problems = append(problems, fmt.Sprintf("%s key file %q holds %d bytes, want at least %d", role, path, len(key), minKeyBytes))
			continue
		}
		if other, dup := seen[string(key)]; dup {
			problems = append(problems, fmt.Sprintf("%s and %s share a key", other, role))
			continue
		}
		seen[string(key)] = role
		keys.byRole[role] = key
		keys.roles = append(keys.roles, role)
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid service keys: %s", strings.Join(problems, "; "))
	}
	return keys, nil
}

// ReadKeyFile reads one service key from path and returns its trimmed base64url text,
// ready to be sent as a bearer token.
//
// It applies the same decode and minimum-length rules LoadKeys does, so a file the
// Allocator presents to a peer is held to the same standard as a file it accepts from
// one. The Allocator uses it for the callback key it presents to Session (D4), which is
// not part of the Keys set: that set is for callers it authenticates, not for keys it
// spends. The returned text is the file's own spelling (padded or not), because the
// peer decodes the token before comparing; only the decoded bytes have to match.
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

// Require returns a handler that admits a request only when it presents a key valid
// for role, and otherwise rejects it through the COM-5 envelope.
//
// A missing or malformed header, or a key that belongs to no configured role, is 401:
// the caller has not identified itself. A key that belongs to a different role is 403:
// the caller identified itself correctly but is not allowed here. The distinction is
// what lets an operator tell "Session has the wrong key" from "a game server is
// calling Session's routes".
//
// The presented key is compared against every configured role's key with
// crypto/subtle.ConstantTimeCompare and no early exit, so response time does not reveal
// which role matched or how much of the key was correct.
func (k *Keys) Require(role Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			apiresp.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "missing or malformed bearer token")
			return
		}

		// The token is the base64url text from the same key file the server loaded, so
		// decode it before comparing. A token that is not valid base64url simply
		// matches nothing; it is not reported separately, so a malformed key and a
		// wrong key look the same to the caller.
		presented, decodeErr := decodeKey(token)

		var matched Role
		found := false
		for _, candidate := range k.roles {
			// Compute the comparison for every role, whatever the previous iteration
			// found, so the loop's timing does not depend on which key matched.
			same := subtle.ConstantTimeCompare(presented, k.byRole[candidate]) == 1
			if same && decodeErr == nil {
				matched = candidate
				found = true
			}
		}

		if !found {
			apiresp.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "unknown key")
			return
		}
		if matched != role {
			apiresp.WriteError(w, r, http.StatusForbidden, "forbidden", "key is not valid for this route")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), roleKey{}, role)))
	})
}

// bearerToken pulls the token out of an Authorization header value. The scheme is
// matched case-insensitively because HTTP defines it that way; everything else — no
// space, a different scheme, an empty token — is a malformed header rather than an
// empty one.
func bearerToken(header string) (string, bool) {
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	return token, true
}
