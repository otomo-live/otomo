package mfa_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/admin_auth/internal/mfa"
)

// rfc6238Secret is the ASCII seed from RFC 6238 Appendix B ("12345678901234567890"),
// which the SHA1 vectors below are computed from.
var rfc6238Secret = []byte("12345678901234567890")

// TestRFC6238SHA1Vectors checks the generator against the published 8-digit vectors.
// 6 digits is the wire form, so the generator is proven at 8 and then the same
// truncation is relied on for 6.
func TestRFC6238SHA1Vectors(t *testing.T) {
	cases := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, c := range cases {
		if got := mfa.Code(rfc6238Secret, time.Unix(c.unix, 0).UTC(), 8); got != c.want {
			t.Errorf("Code(%d, 8) = %s, want %s", c.unix, got, c.want)
		}
	}
}

func TestValidateAcceptsOneStepEitherSide(t *testing.T) {
	secret, err := mfa.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()

	// The code at each of the three accepted steps validates at now, and reports
	// that step back for the replay guard.
	for _, offset := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		code := mfa.Code(secret, now.Add(offset), 6)
		step, ok := mfa.Validate(secret, code, now)
		if !ok {
			t.Errorf("Validate rejected the code %s from offset %s", code, offset)
			continue
		}
		if want := now.Add(offset).Unix() / 30; step != want {
			t.Errorf("step = %d, want %d", step, want)
		}
	}

	// Two steps away on either side is outside the window.
	for _, offset := range []time.Duration{-60 * time.Second, 60 * time.Second} {
		code := mfa.Code(secret, now.Add(offset), 6)
		if _, ok := mfa.Validate(secret, code, now); ok {
			t.Errorf("Validate accepted the out-of-window code %s from offset %s", code, offset)
		}
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	key, err := mfa.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	secret, err := mfa.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}

	sealed, err := mfa.Seal(key, "user-a", secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, secret) {
		t.Error("the ciphertext contains the plaintext secret")
	}

	got, err := mfa.Open(key, "user-a", sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("Open = %x, want %x", got, secret)
	}
}

func TestOpenRejectsWrongAADUser(t *testing.T) {
	key, _ := mfa.GenerateKey()
	secret, _ := mfa.GenerateSecret()

	sealed, err := mfa.Seal(key, "user-a", secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The user id is the GCM additional data: a ciphertext copied to another row
	// must not open even with the same key.
	if _, err := mfa.Open(key, "user-b", sealed); err == nil {
		t.Error("Open accepted a ciphertext sealed for a different user id")
	}
}

func TestOpenRejectsWrongKeyAndTampering(t *testing.T) {
	key, _ := mfa.GenerateKey()
	secret, _ := mfa.GenerateSecret()
	sealed, err := mfa.Seal(key, "user-a", secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	other, _ := mfa.GenerateKey()
	if _, err := mfa.Open(other, "user-a", sealed); err == nil {
		t.Error("Open accepted the ciphertext under a different key")
	}

	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := mfa.Open(key, "user-a", tampered); err == nil {
		t.Error("Open accepted a tampered ciphertext")
	}
}

func TestSealUsesAFreshNonce(t *testing.T) {
	key, _ := mfa.GenerateKey()
	secret, _ := mfa.GenerateSecret()

	first, err := mfa.Seal(key, "user-a", secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := mfa.Seal(key, "user-a", secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Error("two seals of the same secret produced identical ciphertexts")
	}
}

func TestSecretEncodingRoundTrips(t *testing.T) {
	secret, _ := mfa.GenerateSecret()

	encoded := mfa.EncodeSecret(secret)
	if strings.ContainsAny(encoded, "=") {
		t.Errorf("base32 %q carries padding", encoded)
	}
	decoded, err := mfa.DecodeSecret(encoded)
	if err != nil {
		t.Fatalf("DecodeSecret: %v", err)
	}
	if !bytes.Equal(decoded, secret) {
		t.Errorf("DecodeSecret = %x, want %x", decoded, secret)
	}
}

func TestOTPAuthURLFormat(t *testing.T) {
	secret := []byte("12345678901234567890")

	got := mfa.OTPAuthURL("admin@example.com", secret)
	want := "otpauth://totp/Otomo%20Admin:admin@example.com?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=Otomo%20Admin&algorithm=SHA1&digits=6&period=30"
	if got != want {
		t.Errorf("OTPAuthURL =\n %s\nwant\n %s", got, want)
	}
}

func TestRecoveryCodesAreWellFormedAndUnique(t *testing.T) {
	codes, err := mfa.RecoveryCodes(10)
	if err != nil {
		t.Fatalf("RecoveryCodes: %v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("len(codes) = %d, want 10", len(codes))
	}

	seen := map[string]bool{}
	for _, code := range codes {
		if len(code) != 9 || code[4] != '-' {
			t.Errorf("code %q is not xxxx-xxxx shaped", code)
		}
		norm, ok := mfa.NormalizeRecoveryCode(code)
		if !ok {
			t.Errorf("NormalizeRecoveryCode rejected its own output %q", code)
		}
		if seen[norm] {
			t.Errorf("duplicate recovery code %q", code)
		}
		seen[norm] = true
	}

	// The hyphen is optional and the case is insensitive on the way back in.
	norm, ok := mfa.NormalizeRecoveryCode(strings.ToUpper(strings.ReplaceAll(codes[0], "-", "")))
	if !ok || norm != strings.ReplaceAll(codes[0], "-", "") {
		t.Errorf("case/hyphen-insensitive normalization = %q/%v", norm, ok)
	}
}

func TestNormalizeRecoveryCodeRejectsNonCodes(t *testing.T) {
	for _, bad := range []string{"", "123456", "abcd-efg", "abcd-efghi", "abcd-efg!"} {
		if _, ok := mfa.NormalizeRecoveryCode(bad); ok {
			t.Errorf("NormalizeRecoveryCode accepted %q", bad)
		}
	}
}

func TestHashRecoveryCodeIsSHA256OfTheNormalizedCode(t *testing.T) {
	want := sha256.Sum256([]byte("abcdefgh"))
	for _, spelling := range []string{"abcd-efgh", "ABCD-EFGH", " abcdefgh "} {
		got := mfa.HashRecoveryCode(spelling)
		if !bytes.Equal(got, want[:]) {
			t.Errorf("HashRecoveryCode(%q) = %x, want %x", spelling, got, want[:])
		}
	}
}

func TestKeyFileRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "totp.key")
	key, err := mfa.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	if err := mfa.WriteKey(path, key); err != nil {
		t.Fatalf("WriteKey: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 600", info.Mode().Perm())
	}

	got, err := mfa.LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Errorf("LoadKey = %x, want %x", got, key)
	}

	// The file is a base64 line, not a raw byte string.
	raw, _ := os.ReadFile(path)
	if _, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw))); err != nil {
		t.Errorf("key file is not base64: %v", err)
	}
}

func TestWriteKeyRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "totp.key")
	first, _ := mfa.GenerateKey()
	second, _ := mfa.GenerateKey()

	if err := mfa.WriteKey(path, first); err != nil {
		t.Fatalf("first WriteKey: %v", err)
	}
	if err := mfa.WriteKey(path, second); err == nil {
		t.Fatal("WriteKey overwrote an existing file")
	}

	got, err := mfa.LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if !bytes.Equal(got, first) {
		t.Error("the original key was replaced")
	}
}

func TestLoadKeyRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()

	short := filepath.Join(dir, "short.key")
	_ = os.WriteFile(short, []byte(base64.StdEncoding.EncodeToString(make([]byte, 16))), 0o600)
	if _, err := mfa.LoadKey(short); err == nil {
		t.Error("LoadKey accepted a 16-byte key")
	}

	notB64 := filepath.Join(dir, "text.key")
	_ = os.WriteFile(notB64, []byte("not base64!!!"), 0o600)
	if _, err := mfa.LoadKey(notB64); err == nil {
		t.Error("LoadKey accepted a non-base64 file")
	}

	if _, err := mfa.LoadKey(filepath.Join(dir, "missing.key")); err == nil {
		t.Error("LoadKey accepted a missing file")
	}
}
