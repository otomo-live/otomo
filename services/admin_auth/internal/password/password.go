// Package password hashes and verifies staff passwords with argon2id.
//
// The stored form is the standard PHC string so the parameters travel with the hash:
// Verify reads them back out, which is what lets the cost be raised later without
// invalidating existing passwords. Hash always writes the OWASP minimum parameters.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These are the OWASP minimum recommendation (19 MiB, 2
// iterations, 1 lane); a per-password salt and a 32-byte key are standard.
const (
	memory      uint32 = 19456
	iterations  uint32 = 2
	parallelism uint8  = 1
	saltLength         = 16
	keyLength   uint32 = 32

	maxMemory      = 256 * 1024 // KiB
	maxIterations  = 16
	maxParallelism = 16
)

// ErrInvalidHash is returned when a stored PHC string cannot be parsed. It means the
// database row is corrupt or was written by something else — never that the password
// was wrong, which is reported as (false, nil).
var ErrInvalidHash = errors.New("password: malformed argon2id hash")

// Hash returns the argon2id PHC string for pw:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<b64 salt>$<b64 key>
//
// The salt is 16 random bytes from crypto/rand, so the same password hashes
// differently every time.
func Hash(pw string) (string, error) {
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, iterations, memory, parallelism, keyLength)
	return format(memory, iterations, parallelism, salt, key), nil
}

// Verify reports whether pw matches the hash encoded in phc. The parameters are read
// from phc, so a hash created with raised settings still verifies. A malformed phc is
// an error; a well-formed one that does not match is (false, nil).
//
// The comparison is constant-time in the derived key, so a caller cannot learn how
// many leading bytes matched.
func Verify(phc, pw string) (bool, error) {
	memory, iterations, parallelism, salt, want, err := parse(phc)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummy holds the hash the unknown-email path verifies against. It is computed at most
// once, and only when an unknown email is actually seen.
var (
	dummyOnce sync.Once
	dummy     string
)

// Dummy returns a hash to verify an unknown-email login against, so a lookup that
// finds no account still pays the same argon2id cost as one that finds an account.
// The first call computes it; later calls reuse it.
func Dummy() string {
	dummyOnce.Do(func() {
		h, err := Hash("dummy password for constant-time unknown-email logins")
		if err != nil {
			// crypto/rand failing is not a recoverable runtime condition.
			panic("password: cannot build the dummy hash: " + err.Error())
		}
		dummy = h
	})
	return dummy
}

// format renders the PHC string. RawStdEncoding (no padding) is what the PHC spec and
// every other argon2id implementation use, so the string stays interoperable.
func format(memory, iterations uint32, parallelism uint8, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// parse splits and validates a PHC string. It accepts exactly the algorithm, version
// and field order Hash writes; the parameter values themselves are not fixed, which is
// the whole point of storing them.
func parse(phc string) (memory, iterations uint32, parallelism uint8, salt, key []byte, err error) {
	parts := strings.Split(phc, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, key]
	if len(parts) != 6 || parts[0] != "" {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}

	var m, t, p uint64
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	// Upper bounds as well as lower: the parameters come from a database row, and a
	// corrupt or hostile one must not make a single login allocate gigabytes or spin
	// for minutes. 256 MiB, 16 passes and 16 lanes are far above anything Hash writes.
	if m == 0 || m > maxMemory || t == 0 || t > maxIterations || p == 0 || p > maxParallelism {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}

	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	return uint32(m), uint32(t), uint8(p), salt, key, nil
}
