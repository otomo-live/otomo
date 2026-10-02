package servicekey

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// keyBytes builds a distinct, valid-length secret per seed, so two roles never
// accidentally share one in a test.
func keyBytes(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func keyText(seed byte) string {
	return base64.RawURLEncoding.EncodeToString(keyBytes(seed, 32))
}

// writeKeyFile stores text in a fresh file and returns its path.
func writeKeyFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

// loadTwo loads one key per role and returns the set plus the two key texts a caller
// would send as bearer tokens.
func loadTwo(t *testing.T) (*Keys, string, string) {
	t.Helper()

	session, game := keyText(1), keyText(101)
	keys, err := LoadKeys(map[Role]string{
		RoleSession:    writeKeyFile(t, session+"\n"),
		RoleGameServer: writeKeyFile(t, game),
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}
	return keys, session, game
}

// loadThree loads one key per role including the proxy's, so a test can check the new
// role is admitted and kept apart from the two that existed before.
func loadThree(t *testing.T) (*Keys, string, string, string) {
	t.Helper()

	session, game, proxy := keyText(1), keyText(101), keyText(201)
	keys, err := LoadKeys(map[Role]string{
		RoleSession:    writeKeyFile(t, session+"\n"),
		RoleGameServer: writeKeyFile(t, game),
		RoleProxy:      writeKeyFile(t, proxy),
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}
	return keys, session, game, proxy
}

type response struct {
	status int
	body   string
}

func strptr(s string) *string { return &s }

// serve runs h against a request carrying auth, or no Authorization header at all
// when auth is nil.
func serve(h http.Handler, auth *string) response {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if auth != nil {
		req.Header.Set("Authorization", *auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return response{status: rec.Code, body: rec.Body.String()}
}

// errorCode pulls the COM-5 code out of a response body.
func errorCode(t *testing.T, body string) string {
	t.Helper()

	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("body is not the COM-5 envelope: %v (%s)", err, body)
	}
	return env.Error.Code
}

// TestRequireAdmitsTheMatchingRole checks the happy path for both roles, including
// that Require puts the verified role on the context for the handler.
func TestRequireAdmitsTheMatchingRole(t *testing.T) {
	keys, session, game := loadTwo(t)

	for _, tc := range []struct {
		role  Role
		token string
	}{
		{RoleSession, session},
		{RoleGameServer, game},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			var got Role
			var present bool
			h := keys.Require(tc.role, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, present = RoleFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			}))

			resp := serve(h, strptr("Bearer "+tc.token))
			if resp.status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.body)
			}
			if !present || got != tc.role {
				t.Errorf("RoleFrom = (%q, %v), want (%q, true)", got, present, tc.role)
			}
		})
	}
}

// TestRequireRejectsTheWrongRole checks that a real key used on the wrong role is 403,
// not 401: the caller is known, it is just not allowed here.
func TestRequireRejectsTheWrongRole(t *testing.T) {
	keys, _, game := loadTwo(t)

	h := keys.Require(RoleSession, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	resp := serve(h, strptr("Bearer "+game))
	if resp.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", resp.status, resp.body)
	}
	if code := errorCode(t, resp.body); code != "forbidden" {
		t.Errorf("code = %q, want forbidden", code)
	}
}

// TestRequireAdmitsProxyRole checks the new role is wired like the others: the proxy's
// key is admitted on the proxy route and RoleFrom reports it.
func TestRequireAdmitsProxyRole(t *testing.T) {
	keys, _, _, proxy := loadThree(t)

	var got Role
	var present bool
	h := keys.Require(RoleProxy, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, present = RoleFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	if resp := serve(h, strptr("Bearer "+proxy)); resp.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.body)
	}
	if !present || got != RoleProxy {
		t.Errorf("RoleFrom = (%q, %v), want (%q, true)", got, present, RoleProxy)
	}
}

// TestProxyRoleStaysDistinct checks the role split from both sides: an existing role's
// key is 403 on the proxy route, and the proxy's key is 403 on an existing route.
func TestProxyRoleStaysDistinct(t *testing.T) {
	keys, session, game, proxy := loadThree(t)

	proxyHandler := keys.Require(RoleProxy, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	sessionHandler := keys.Require(RoleSession, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	cases := []struct {
		name  string
		h     http.Handler
		token string
	}{
		{"session key on proxy route", proxyHandler, session},
		{"gameserver key on proxy route", proxyHandler, game},
		{"proxy key on session route", sessionHandler, proxy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := serve(tc.h, strptr("Bearer "+tc.token))
			if resp.status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", resp.status, resp.body)
			}
			if code := errorCode(t, resp.body); code != "forbidden" {
				t.Errorf("code = %q, want forbidden", code)
			}
		})
	}
}

// TestLoadKeysNamesTheProxyRole is the missing/unreadable case for the new key file:
// the error must name the role so an operator knows which secret to fix.
func TestLoadKeysNamesTheProxyRole(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"missing", filepath.Join(t.TempDir(), "absent")},
		// A directory is readable by name but not by content, so os.ReadFile fails on
		// it the way a key file with the wrong permissions would.
		{"unreadable", t.TempDir()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadKeys(map[Role]string{RoleProxy: tc.path})
			if err == nil {
				t.Fatalf("LoadKeys accepted an %s proxy key file", tc.name)
			}
			if !strings.Contains(err.Error(), "proxy") {
				t.Errorf("error does not name the proxy role: %v", err)
			}
		})
	}
}

// TestRequireRejectsUnknownAndMalformedKeys covers every way a request can fail to
// identify itself. All of them are 401 with the same code, so a caller cannot tell a
// miss from a malformed header.
func TestRequireRejectsUnknownAndMalformedKeys(t *testing.T) {
	keys, _, _ := loadTwo(t)

	h := keys.Require(RoleSession, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name string
		auth *string
	}{
		{"missing header", nil},
		{"basic scheme", strptr("Basic " + keyText(200))},
		{"bearer with no key", strptr("Bearer ")},
		{"bearer with only spaces", strptr("Bearer    ")},
		{"unknown key", strptr("Bearer " + keyText(200))},
		{"not base64url", strptr("Bearer not a key!!!")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := serve(h, tc.auth)
			if resp.status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", resp.status, resp.body)
			}
			if code := errorCode(t, resp.body); code != "unauthorized" {
				t.Errorf("code = %q, want unauthorized", code)
			}
		})
	}
}

// TestLoadKeysRejectsDuplicateKeys checks the one cross-role rule: two roles must not
// share a key, or the role split means nothing.
func TestLoadKeysRejectsDuplicateKeys(t *testing.T) {
	shared := keyText(7)
	_, err := LoadKeys(map[Role]string{
		RoleSession:    writeKeyFile(t, shared),
		RoleGameServer: writeKeyFile(t, shared),
	})
	if err == nil {
		t.Fatal("LoadKeys accepted the same key for two roles")
	}
	if !strings.Contains(err.Error(), "share a key") {
		t.Errorf("error does not explain the duplicate: %v", err)
	}
}

// TestLoadKeysRejectsBadKeys covers the per-file failures: unreadable, empty, too
// short and not base64url.
func TestLoadKeysRejectsBadKeys(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"whitespace only", "\n\t  \n"},
		{"too short", base64.RawURLEncoding.EncodeToString(keyBytes(1, 31))},
		{"not base64url", "this is not base64url!!!"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadKeys(map[Role]string{RoleSession: writeKeyFile(t, tc.text)}); err == nil {
				t.Fatalf("LoadKeys accepted %s", tc.name)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadKeys(map[Role]string{RoleSession: filepath.Join(t.TempDir(), "absent")}); err == nil {
			t.Fatal("LoadKeys accepted a missing key file")
		}
	})
}

// TestLoadKeysAcceptsPaddedBase64URL pins the one encoding tolerance: a key written
// with URL-safe padding decodes to the same bytes as one written without it.
func TestLoadKeysAcceptsPaddedBase64URL(t *testing.T) {
	padded := base64.URLEncoding.EncodeToString(keyBytes(9, 32))
	if !strings.HasSuffix(padded, "=") {
		t.Fatalf("test key %q is not padded", padded)
	}
	keys, err := LoadKeys(map[Role]string{RoleSession: writeKeyFile(t, padded)})
	if err != nil {
		t.Fatalf("LoadKeys rejected a padded key: %v", err)
	}

	h := keys.Require(RoleSession, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if resp := serve(h, strptr("Bearer "+padded)); resp.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.body)
	}
}

// TestReadKeyFile checks the single-file helper the callback key is loaded with. It
// applies the same rules as LoadKeys and returns the file's own spelling, trimmed.
func TestReadKeyFile(t *testing.T) {
	text := keyText(21)
	got, err := ReadKeyFile(writeKeyFile(t, text+"\n"))
	if err != nil {
		t.Fatalf("ReadKeyFile: %v", err)
	}
	if got != text {
		t.Errorf("ReadKeyFile = %q, want %q", got, text)
	}

	cases := []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"whitespace only", "\n\t  \n"},
		{"too short", base64.RawURLEncoding.EncodeToString(keyBytes(1, 31))},
		{"not base64url", "this is not base64url!!!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadKeyFile(writeKeyFile(t, tc.text)); err == nil {
				t.Fatalf("ReadKeyFile accepted %s", tc.name)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := ReadKeyFile(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Fatal("ReadKeyFile accepted a missing file")
		}
	})
}
