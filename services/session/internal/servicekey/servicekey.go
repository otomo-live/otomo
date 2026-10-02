// Package servicekey authenticates Session's internal callers with static keys (design
// decision D4 in design/13-player-plane-plan.md; the same scheme as
// services/allocator/internal/servicekey and services/patch/internal/servicekey).
//
// A caller presents its key as Authorization: Bearer <key>. Each key lives in its own
// file under deploy/secrets/service_keys/, mounted read-only, and is valid for one role
// only. The key file is named for the service that accepts it and the one that presents
// it: session_allocator.key is accepted by Session and presented by the Allocator.
//
// A missing, malformed or unknown key is 401. A key that belongs to another configured
// role is 403. The presented key is compared with every configured key in constant time,
// with no early exit.
package servicekey

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/otomo-live/otomo/services/session/internal/api"
)

// Role names a caller.
type Role string

// RoleAllocator is the Allocator telling Session that an allocation ended (LB-4,
// design/14-launch-handoff.md §4.4).
const RoleAllocator Role = "allocator"

// minKeyBytes is the shortest decoded key accepted: a generated key is 32 random bytes.
const minKeyBytes = 32

// Keys is the set of accepted keys, one per role. Build it with LoadKeys; it is read-only
// afterwards and safe for concurrent use.
type Keys struct {
	byRole map[Role][]byte
	roles  []Role // fixed order, so every request compares in the same order
}

// decodeKey turns a key file's text into its bytes. Padded and unpadded base64url are
// both accepted.
func decodeKey(text string) ([]byte, error) {
	text = strings.TrimRight(strings.TrimSpace(text), "=")
	return base64.RawURLEncoding.DecodeString(text)
}

// LoadKeys reads one key per role. Every problem is reported at once, and key material
// never appears in an error. Two roles sharing a key is refused.
func LoadKeys(paths map[Role]string) (*Keys, error) {
	roles := make([]Role, 0, len(paths))
	for role := range paths {
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })

	var problems []string
	keys := &Keys{byRole: make(map[Role][]byte, len(roles))}
	seen := map[string]Role{}
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

// Require admits a request only when it presents the key for role. A nil *Keys admits
// nobody, so a Session started without keys fails closed.
func (k *Keys) Require(role Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			api.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "missing or malformed bearer token")
			return
		}
		presented, decodeErr := decodeKey(token)

		var matched Role
		found := false
		if k != nil {
			for _, candidate := range k.roles {
				same := subtle.ConstantTimeCompare(presented, k.byRole[candidate]) == 1
				if same && decodeErr == nil {
					matched, found = candidate, true
				}
			}
		}
		if !found {
			api.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "unknown key")
			return
		}
		if matched != role {
			api.WriteError(w, r, http.StatusForbidden, "forbidden", "key is not valid for this route")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken returns the token of an "Authorization: Bearer <token>" header.
func bearerToken(header string) (string, bool) {
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	return token, token != ""
}
