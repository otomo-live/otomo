package password_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/otomo-live/otomo/services/admin_auth/internal/password"
)

// TestHashWritesConfiguredParamsAndAFreshSalt pins the PHC string Hash produces to the
// configured OWASP parameters and proves two hashes of the same password never share a
// salt or a derived key.
func TestHashWritesConfiguredParamsAndAFreshSalt(t *testing.T) {
	first, err := password.Hash("same password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	second, err := password.Hash("same password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	fields := func(phc string) []string { return strings.Split(phc, "$") }
	f1, f2 := fields(first), fields(second)
	if len(f1) != 6 || f1[1] != "argon2id" || f1[2] != "v=19" {
		t.Fatalf("hash %q is not the argon2id v=19 PHC shape", first)
	}
	if f1[3] != "m=19456,t=2,p=1" {
		t.Errorf("params = %q, want m=19456,t=2,p=1", f1[3])
	}
	if f1[4] == f2[4] {
		t.Errorf("salt reused between two hashes: %q", f1[4])
	}
	if f1[5] == f2[5] {
		t.Errorf("derived key reused between two hashes: %q", f1[5])
	}
}

// TestDummyVerifiesInComparableTime proves the unknown-email path costs the same as a
// real account: verifying a wrong password against Dummy takes roughly as long as
// verifying it against a real stored hash. The two paths are measured alternately so a
// slow patch of the machine lands on both samples, and the bound is a loose 2x ratio —
// this flags a zero-work dummy, not scheduler noise.
func TestDummyVerifiesInComparableTime(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test skipped under -short")
	}

	dummy := password.Dummy()
	real, err := password.Hash("the real password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	timeOne := func(phc string) time.Duration {
		start := time.Now()
		ok, err := password.Verify(phc, "definitely wrong")
		d := time.Since(start)
		if err != nil || ok {
			t.Fatalf("Verify = %v/%v, want false/nil", ok, err)
		}
		return d
	}

	// Warm up both paths and the sync.Once inside Dummy before timing anything.
	for i := 0; i < 3; i++ {
		timeOne(dummy)
		timeOne(real)
	}

	// Interleave the two paths so a slow patch of the machine lands on both samples.
	const n = 15
	dummySamples := make([]time.Duration, n)
	realSamples := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		dummySamples[i] = timeOne(dummy)
		realSamples[i] = timeOne(real)
	}
	sort.Slice(dummySamples, func(i, j int) bool { return dummySamples[i] < dummySamples[j] })
	sort.Slice(realSamples, func(i, j int) bool { return realSamples[i] < realSamples[j] })
	dummyMedian, realMedian := dummySamples[n/2], realSamples[n/2]

	slow, fast := dummyMedian, realMedian
	if slow < fast {
		slow, fast = fast, slow
	}
	if fast <= 0 || slow > 2*fast {
		t.Errorf("Dummy verify median %s vs real %s is more than 2x apart", dummyMedian, realMedian)
	}
}

func TestHashVerifyRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"

	phc, err := password.Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	for _, want := range []string{"$argon2id$v=19$m=19456,t=2,p=1$"} {
		if !strings.HasPrefix(phc, want) {
			t.Errorf("hash = %q, want prefix %q", phc, want)
		}
	}

	ok, err := password.Verify(phc, pw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Error("Verify returned false for the password that was hashed")
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	phc, err := password.Hash("the real one")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	ok, err := password.Verify(phc, "not the real one")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Error("Verify accepted a wrong password")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	valid, err := password.Hash("pw")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	// A valid hash with its key field blanked, so the empty key must be rejected.
	lastDollar := strings.LastIndexByte(valid, '$')
	emptyKey := valid[:lastDollar+1]

	// A valid hash whose salt field is not base64.
	fields := strings.Split(valid, "$")
	fields[4] = "not*base64"
	badSalt := strings.Join(fields, "$")

	cases := map[string]string{
		"empty":           "",
		"not phc":         "plaintext",
		"wrong algorithm": strings.Replace(valid, "$argon2id$", "$argon2i$", 1),
		"wrong version":   strings.Replace(valid, "$v=19$", "$v=18$", 1),
		"non-numeric m":   strings.Replace(valid, "m=19456", "m=lots", 1),
		"zero iterations": strings.Replace(valid, "t=2", "t=0", 1),
		"bad salt b64":    badSalt,
		"empty key":       emptyKey,
		"missing fields":  "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA",
	}

	for name, phc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := password.Verify(phc, "pw")
			if !errors.Is(err, password.ErrInvalidHash) {
				t.Errorf("Verify(%q) error = %v, want password.ErrInvalidHash", phc, err)
			}
		})
	}
}

// TestVerifyParsesParamsFromTheHash proves the parameters are read back out of the
// PHC string: a hash made with settings other than the current Hash defaults must
// still verify.
func TestVerifyParsesParamsFromTheHash(t *testing.T) {
	const (
		memory      = 65536
		iterations  = 3
		parallelism = 4
		pw          = "a password from an older policy"
	)
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(pw), salt, iterations, memory, parallelism, 32)
	phc := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))

	ok, err := password.Verify(phc, pw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Error("Verify did not honour the parameters stored in the hash")
	}
}

func TestDummyIsStableAndValid(t *testing.T) {
	first := password.Dummy()
	if second := password.Dummy(); first != second {
		t.Error("Dummy returned a different hash on the second call")
	}
	if !strings.HasPrefix(first, "$argon2id$") {
		t.Errorf("Dummy = %q, want an argon2id PHC string", first)
	}
}

// A stored hash whose parameters are absurd is rejected before any work is done, so a
// corrupt row cannot turn one login into a multi-gigabyte allocation.
func TestVerifyRejectsExcessiveParameters(t *testing.T) {
	salt := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	key := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	for _, params := range []string{"m=4194304,t=2,p=1", "m=19456,t=1000,p=1", "m=19456,t=2,p=200"} {
		phc := fmt.Sprintf("$argon2id$v=%d$%s$%s$%s", argon2.Version, params, salt, key)
		if _, err := password.Verify(phc, "x"); !errors.Is(err, password.ErrInvalidHash) {
			t.Errorf("Verify with %s: err = %v, want ErrInvalidHash", params, err)
		}
	}
}
