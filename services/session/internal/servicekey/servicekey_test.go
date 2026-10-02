package servicekey

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newKey writes a generated key to a file the way generate-secrets.sh does (32 random
// bytes, unpadded base64url) and returns its path and text.
func newKey(t *testing.T) (path, text string) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	text = base64.RawURLEncoding.EncodeToString(raw)
	path = filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, text
}

func call(h http.Handler, auth string) int {
	req := httptest.NewRequest(http.MethodPost, "/internal/session/allocations/x/ended", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestRequire(t *testing.T) {
	allocPath, allocKey := newKey(t)
	otherPath, otherKey := newKey(t)
	keys, err := LoadKeys(map[Role]string{RoleAllocator: allocPath, "other": otherPath})
	if err != nil {
		t.Fatal(err)
	}
	h := keys.Require(RoleAllocator, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	_, stranger := newKey(t)
	for _, tt := range []struct {
		name, auth string
		want       int
	}{
		{"the allocator's key", "Bearer " + allocKey, http.StatusNoContent},
		{"the key with padding", "Bearer " + allocKey + "=", http.StatusNoContent},
		{"a lower-case scheme", "bearer " + allocKey, http.StatusNoContent},
		{"no header", "", http.StatusUnauthorized},
		{"no scheme", allocKey, http.StatusUnauthorized},
		{"basic auth", "Basic " + allocKey, http.StatusUnauthorized},
		{"an unknown key", "Bearer " + stranger, http.StatusUnauthorized},
		{"not base64url", "Bearer !!!", http.StatusUnauthorized},
		{"a truncated key", "Bearer " + allocKey[:20], http.StatusUnauthorized},
		{"another role's key", "Bearer " + otherKey, http.StatusForbidden},
	} {
		if got := call(h, tt.auth); got != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestNoKeysAdmitNobody(t *testing.T) {
	var keys *Keys
	_, text := newKey(t)
	h := keys.Require(RoleAllocator, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request got through with no keys configured")
	}))
	if got := call(h, "Bearer "+text); got != http.StatusUnauthorized {
		t.Errorf("no keys configured = %d, want 401", got)
	}
}

func TestLoadKeysRefusesBadFiles(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte(base64.RawURLEncoding.EncodeToString(make([]byte, 16))), 0o600); err != nil {
		t.Fatal(err)
	}
	notB64 := filepath.Join(dir, "garbage")
	if err := os.WriteFile(notB64, []byte("this is not a key!"), 0o600); err != nil {
		t.Fatal(err)
	}
	same, _ := newKey(t)

	for name, paths := range map[string]map[Role]string{
		"missing file": {RoleAllocator: filepath.Join(dir, "absent")},
		"short key":    {RoleAllocator: short},
		"not base64":   {RoleAllocator: notB64},
		"shared key":   {RoleAllocator: same, "other": same},
	} {
		_, err := LoadKeys(paths)
		if err == nil {
			t.Errorf("%s: LoadKeys accepted it", name)
			continue
		}
		if strings.Contains(err.Error(), "this is not a key") {
			t.Errorf("%s: the error repeats the key file's contents: %v", name, err)
		}
	}
}
