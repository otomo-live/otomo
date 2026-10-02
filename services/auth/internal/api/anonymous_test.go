package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/auth/internal/api"
	"github.com/otomo-live/otomo/services/auth/internal/token"
)

// fakeAccounts hands out one stable UUID per (method, externalID), like the real
// find-or-create, and records what it was asked for.
type fakeAccounts struct {
	mu    sync.Mutex
	ids   map[string]string
	calls []string // method + "/" + externalID
	err   error
	fixed string // when set, returned for every binding
}

func (f *fakeAccounts) FindOrCreateAccount(_ context.Context, method, externalID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+"/"+externalID)
	if f.err != nil {
		return "", false, f.err
	}
	if f.fixed != "" {
		return f.fixed, false, nil
	}
	if f.ids == nil {
		f.ids = map[string]string{}
	}
	key := method + "/" + externalID
	_, known := f.ids[key]
	if !known {
		f.ids[key] = uuid.New().String()
	}
	return f.ids[key], !known, nil
}

type anonymousFixture struct {
	handler  http.HandlerFunc
	accounts *fakeAccounts
	refresh  *fakeRefresh
	pub      ed25519.PublicKey
	logs     *bytes.Buffer
}

func newAnonymousFixture(t *testing.T) *anonymousFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	f := &anonymousFixture{accounts: &fakeAccounts{}, refresh: newFakeRefresh(), pub: pub, logs: &bytes.Buffer{}}
	f.handler = api.Anonymous(api.AnonymousDeps{
		Accounts:   f.accounts,
		Refresh:    f.refresh,
		Issuer:     testSigner(priv),
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 720 * time.Hour,
		Logger:     slog.New(slog.NewJSONHandler(f.logs, nil)),
	})
	return f
}

func (f *anonymousFixture) post(body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/auth/anonymous", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	f.handler.ServeHTTP(w, r)
	return w
}

// login posts a device login that must succeed and returns the token's subject.
func (f *anonymousFixture) login(t *testing.T, deviceID string) string {
	t.Helper()
	w := f.post(`{"device_id":"` + deviceID + `"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", w.Code, w.Body)
	}
	body := decodeLogin(t, w)
	if body.RefreshToken == "" {
		t.Error("login returned no refresh_token")
	}
	return subjectOf(t, f.pub, body.AccessToken)
}

// loginBody is the success shape shared by /auth/anonymous and /auth/refresh.
type loginBody struct {
	SchemaVersion int    `json:"schema_version"`
	AccessToken   string `json:"access_token"`
	ExpiresIn     int64  `json:"expires_in"`
	RefreshToken  string `json:"refresh_token"`
}

// decodeLogin decodes a login or refresh body and checks every field whose value is
// fixed by the contract, including the exact key set: no field may be added or
// dropped without the docs and schema_version saying so.
func decodeLogin(t *testing.T, w *httptest.ResponseRecorder) loginBody {
	t.Helper()
	var body loginBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	if body.ExpiresIn != 900 {
		t.Errorf("expires_in = %d, want 900", body.ExpiresIn)
	}
	if body.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", body.SchemaVersion)
	}
	// Tokens only: the services hand-off was dropped (D2), so a "services" key coming
	// back would be a regression, not an extra.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	if got := keys(top); got != "access_token,expires_in,refresh_token,schema_version" {
		t.Errorf("top-level keys = %s, want the four token fields and nothing else", got)
	}
	return body
}

func keys(m map[string]json.RawMessage) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// subjectOf verifies an access token against pub with the player domain's rules and
// returns its sub.
func subjectOf(t *testing.T, pub ed25519.PublicKey, access string) string {
	t.Helper()
	claims := &jwt.RegisteredClaims{}
	if _, err := jwt.ParseWithClaims(access, claims,
		func(*jwt.Token) (any, error) { return pub, nil },
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer("https://auth.otomo.internal"),
		jwt.WithAudience("otomo:player"),
		jwt.WithExpirationRequired(),
	); err != nil {
		t.Fatalf("access token does not verify: %v", err)
	}
	return claims.Subject
}

func testSigner(priv ed25519.PrivateKey) *token.Signer {
	return &token.Signer{
		Kid: "test", PrivateKey: priv,
		Issuer: "https://auth.otomo.internal", Audience: "otomo:player",
		TTL: 15 * time.Minute,
	}
}

// newDeviceID is what the SDK generates: 32 random bytes as base64url.
func newDeviceID(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func TestAnonymousIssuesATokenForTheDeviceAccount(t *testing.T) {
	f := newAnonymousFixture(t)
	device := newDeviceID(t)

	w := f.post(`{"device_id":"` + device + `"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", w.Code, w.Body)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	sub := f.login(t, device)
	if want := f.accounts.ids["device/"+hexSHA256(device)]; sub != want {
		t.Errorf("sub = %q, want the store's account id %q", sub, want)
	}

	// Each login starts its own refresh family, owned by the same account, with the
	// 30-day expiry, and stores only the token's hash.
	body := decodeLogin(t, w)
	row, ok := f.refresh.lookup(body.RefreshToken)
	if !ok {
		t.Fatal("the returned refresh_token was not stored")
	}
	if row.account != sub {
		t.Errorf("refresh token belongs to %q, want %q", row.account, sub)
	}
	if d := time.Until(row.expires); d < 719*time.Hour || d > 721*time.Hour {
		t.Errorf("refresh token expires in %v, want about 720h", d)
	}
	if len(f.refresh.families()) != 2 {
		t.Errorf("two logins made %d refresh families, want 2", len(f.refresh.families()))
	}

	// The store is keyed on the hash, never the device_id itself.
	for _, call := range f.accounts.calls {
		if strings.Contains(call, device) {
			t.Errorf("the raw device_id reached the store: %q", call)
		}
		if call != "device/"+hexSHA256(device) {
			t.Errorf("store called with %q, want device/<hex sha256>", call)
		}
	}
}

func TestAnonymousSameDeviceGetsTheSameSubject(t *testing.T) {
	f := newAnonymousFixture(t)
	a, b := newDeviceID(t), newDeviceID(t)

	first, second := f.login(t, a), f.login(t, a)
	if first != second {
		t.Errorf("same device_id produced two subjects: %q and %q", first, second)
	}
	if other := f.login(t, b); other == first {
		t.Errorf("two different device_ids share the subject %q", other)
	}
}

func TestAnonymousAcceptsTheDeviceIDLengthBounds(t *testing.T) {
	f := newAnonymousFixture(t)
	for _, id := range []string{
		strings.Repeat("a", 22),
		strings.Repeat("Z", 128),
		"Ab09_-" + strings.Repeat("x", 16),
	} {
		f.login(t, id)
	}
}

func TestAnonymousRejectsBadBodiesWithValidationFailed(t *testing.T) {
	valid := strings.Repeat("a", 43)
	for name, body := range map[string]string{
		"empty body":          "",
		"not JSON":            "device_id=" + valid,
		"JSON null":           "null",
		"JSON array":          `["` + valid + `"]`,
		"missing field":       `{}`,
		"empty device_id":     `{"device_id":""}`,
		"21 characters":       `{"device_id":"` + strings.Repeat("a", 21) + `"}`,
		"129 characters":      `{"device_id":"` + strings.Repeat("a", 129) + `"}`,
		"plus sign":           `{"device_id":"` + strings.Repeat("a", 30) + `+"}`,
		"slash":               `{"device_id":"` + strings.Repeat("a", 30) + `/"}`,
		"padding":             `{"device_id":"` + strings.Repeat("a", 30) + `="}`,
		"space":               `{"device_id":"` + strings.Repeat("a", 15) + " " + strings.Repeat("a", 15) + `"}`,
		"non-ASCII":           `{"device_id":"` + strings.Repeat("a", 30) + `é"}`,
		"number":              `{"device_id":12345678901234567890123}`,
		"trailing object":     `{"device_id":"` + valid + `"}{"device_id":"` + valid + `"}`,
		"trailing garbage":    `{"device_id":"` + valid + `"} x`,
		"body over 4 KiB":     `{"device_id":"` + valid + `"` + strings.Repeat(" ", 5000) + `}`,
		"unterminated object": `{"device_id":"` + valid + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newAnonymousFixture(t)
			w := f.post(body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s; want 400", w.Code, w.Body)
			}
			if code := errorCode(t, w); code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
			if len(f.accounts.calls) != 0 {
				t.Errorf("the store was called for an invalid body: %v", f.accounts.calls)
			}
		})
	}
}

func TestAnonymousStoreFailureIsAnOpaque500(t *testing.T) {
	f := newAnonymousFixture(t)
	f.accounts.err = errors.New(`duplicate key value violates "identity_binding_pkey"`)
	device := newDeviceID(t)

	w := f.post(`{"device_id":"` + device + `"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if code := errorCode(t, w); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
	if strings.Contains(w.Body.String(), "identity_binding") {
		t.Errorf("the store error leaked into the response: %s", w.Body)
	}
	if !strings.Contains(f.logs.String(), "identity_binding") {
		t.Error("the store error was not logged")
	}
	if strings.Contains(f.logs.String(), device) {
		t.Error("the device_id was logged")
	}
}

func TestAnonymousRefusesToIssueForANonUUIDAccount(t *testing.T) {
	f := newAnonymousFixture(t)
	f.accounts.fixed = "not-a-uuid"

	w := f.post(`{"device_id":"` + newDeviceID(t) + `"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "access_token") {
		t.Errorf("a token was issued for a non-UUID subject: %s", w.Body)
	}
}

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
