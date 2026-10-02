// Package mfa implements the staff second factor: RFC 6238 TOTP codes, the
// AES-256-GCM sealing of a stored secret, one-time recovery codes, and the on-disk
// symmetric key that protects the secrets.
//
// The package is deliberately dependency-free — TOTP, HMAC-SHA1 and AES-GCM all come
// from the standard library — so the policy decision to add a second factor does not
// add a supply-chain surface. It knows nothing about HTTP or the database: callers
// pass a time in, get a code or a validity verdict out, and own every error response.
package mfa

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// KeySize is the size of the symmetric key that seals TOTP secrets. AES-256
	// requires exactly 32 bytes.
	KeySize = 32

	// SecretSize is the raw entropy of one TOTP seed, as RFC 4226 recommends and
	// RFC 6238 Appendix B uses (20 bytes).
	SecretSize = 20

	// Period and Digits are the TOTP parameters published in otpauth_url. The
	// contract fixes them, so they are constants rather than configuration.
	Period = 30 * time.Second
	Digits = 6

	// keyPerms keeps the TOTP key readable only by its owner, matching the signing
	// key file.
	keyPerms = fs.FileMode(0o600)
)

// ErrKeyExists reports that a key file was already present, so genkey refuses to
// overwrite it.
var ErrKeyExists = errors.New("key file already exists")

// LoadKey reads a base64 (standard encoding) one-line key file and requires it to
// decode to exactly KeySize bytes. Every rejection names the path and what was found
// so an operator with several secret files can tell which one is wrong.
func LoadKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read TOTP key %s: %w", path, err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("decode TOTP key %s: %w", path, err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("TOTP key %s is %d bytes, want %d", path, len(key), KeySize)
	}
	return key, nil
}

// GenerateKey returns KeySize fresh random bytes.
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate TOTP key: %w", err)
	}
	return key, nil
}

// EncodeKey renders a key as the one-line base64 form LoadKey reads.
func EncodeKey(key []byte) string {
	return base64.StdEncoding.EncodeToString(key)
}

// WriteKey writes key as a base64 line with keyPerms, refusing to overwrite an
// existing file. O_EXCL makes the refusal atomic: two concurrent genkey runs cannot
// both believe the path was free.
func WriteKey(path string, key []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyPerms)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s: %w", path, ErrKeyExists)
	}
	if err != nil {
		return fmt.Errorf("write TOTP key %s: %w", path, err)
	}
	if _, err := f.WriteString(EncodeKey(key) + "\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("write TOTP key %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write TOTP key %s: %w", path, err)
	}
	return nil
}

// Seal encrypts secret for userID under key. The returned blob is the random 12-byte
// GCM nonce followed by the ciphertext. userID is authenticated as additional data,
// so a ciphertext moved to another staff row does not open.
func Seal(key []byte, userID string, secret []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal TOTP secret: read nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, secret, []byte(userID)), nil
}

// Open reverses Seal for userID. A wrong key, a truncated blob or the wrong user id
// all fail: the user id is the GCM additional data and a mismatch fails the tag.
func Open(key []byte, userID string, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("open TOTP secret: ciphertext is shorter than the nonce")
	}
	secret, err := gcm.Open(nil, sealed[:ns], sealed[ns:], []byte(userID))
	if err != nil {
		return nil, fmt.Errorf("open TOTP secret: %w", err)
	}
	return secret, nil
}

// newGCM builds the AES-256-GCM AEAD, rejecting a key of the wrong size with a clear
// error rather than AES's opaque one.
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("TOTP key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("build AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build GCM: %w", err)
	}
	return gcm, nil
}

// GenerateSecret returns one fresh SecretSize-byte TOTP seed.
func GenerateSecret() ([]byte, error) {
	secret := make([]byte, SecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate TOTP secret: %w", err)
	}
	return secret, nil
}

// EncodeSecret renders a seed as unpadded uppercase base32, the form the otpauth URI
// and the enrollment response use.
func EncodeSecret(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

// DecodeSecret reverses EncodeSecret. It accepts lowercase and missing padding so a
// code typed by a human round-trips.
func DecodeSecret(encoded string) ([]byte, error) {
	s := strings.ToUpper(strings.TrimSpace(encoded))
	s = strings.TrimRight(s, "=")
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode TOTP secret: %w", err)
	}
	return secret, nil
}

// Code returns the digits-digit TOTP for secret at time t: HMAC-SHA1 over the 30s
// step counter, RFC 4226 dynamic truncation, zero-padded to digits.
func Code(secret []byte, t time.Time, digits int) string {
	counter := uint64(t.Unix() / int64(Period/time.Second))

	mac := hmac.New(sha1.New, secret)
	_ = binary.Write(mac, binary.BigEndian, counter)
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%mod)
}

// Validate checks code against secret at now, accepting the previous, current and next
// 30s step (a clock skew of one step in either direction). It returns the step the
// matching code belongs to, which the caller stores to enforce one-time use, and
// whether any candidate matched. Comparison is constant-time.
func Validate(secret []byte, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	current := now.Unix() / int64(Period/time.Second)
	for _, step := range []int64{current - 1, current, current + 1} {
		if step < 0 {
			continue
		}
		want := Code(secret, time.Unix(step*int64(Period/time.Second), 0).UTC(), Digits)
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// OTPAuthURL builds the enrollment URI authenticator apps scan. The label and issuer
// are the literal "Otomo Admin" display name; the secret is base32. The query string
// is assembled by hand so the spaces are percent-encoded as %20, which the contract's
// example fixes, rather than the + that url.Values would emit.
func OTPAuthURL(email string, secret []byte) string {
	return fmt.Sprintf(
		"otpauth://totp/Otomo%%20Admin:%s?secret=%s&issuer=Otomo%%20Admin&algorithm=SHA1&digits=%d&period=%d",
		url.PathEscape(email), EncodeSecret(secret), Digits, int(Period/time.Second),
	)
}

// recoveryAlphabet is lowercase base32 with the characters most easily confused when
// read aloud or off paper removed: i, l and o. The digits 0 and 1 are absent from
// base32 already.
const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz234567"

// recoveryGroups is how many 4-character groups one code has; the display form joins
// them with a hyphen ("xxxx-xxxx").
const recoveryGroups = 2

// RecoveryCodes returns n fresh one-time codes in the display form xxxx-xxxx. Only the
// caller's sha256 hashes should be stored; see HashRecoveryCode.
func RecoveryCodes(n int) ([]string, error) {
	codes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		for g := 0; g < recoveryGroups; g++ {
			if g > 0 {
				b.WriteByte('-')
			}
			for j := 0; j < 4; j++ {
				idx, err := randIndex(len(recoveryAlphabet))
				if err != nil {
					return nil, err
				}
				b.WriteByte(recoveryAlphabet[idx])
			}
		}
		codes = append(codes, b.String())
	}
	return codes, nil
}

// randIndex returns a uniformly random index in [0,n) without modulo bias.
func randIndex(n int) (int, error) {
	if n <= 0 || n > 256 {
		return 0, fmt.Errorf("randIndex: invalid bound %d", n)
	}
	limit := 256 - (256 % n)
	for {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return 0, fmt.Errorf("recovery code: read randomness: %w", err)
		}
		if int(b[0]) < limit {
			return int(b[0]) % n, nil
		}
	}
}

// NormalizeRecoveryCode lowercases a code and strips the optional hyphens and any
// surrounding whitespace. It reports false when the result is not exactly the right
// length or contains a character outside the alphabet, so a TOTP-shaped input is
// never misread as a recovery code.
func NormalizeRecoveryCode(code string) (string, bool) {
	code = strings.ToLower(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	if len(code) != recoveryGroups*4 {
		return "", false
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(recoveryAlphabet, rune(code[i])) {
			return "", false
		}
	}
	return code, true
}

// HashRecoveryCode returns the sha256 of the normalized code. Only this digest is
// stored; the display value is shown to the user once. Normalizing here means the
// stored hash does not depend on the case or whether the user typed the hyphen, so a
// code verifies however it was entered.
func HashRecoveryCode(code string) []byte {
	normalized, ok := NormalizeRecoveryCode(code)
	if !ok {
		normalized = code
	}
	sum := sha256.Sum256([]byte(normalized))
	return sum[:]
}
