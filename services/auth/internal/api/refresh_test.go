package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/otomo-live/otomo/services/auth/internal/api"
	"github.com/otomo-live/otomo/services/auth/internal/store"
)

type refreshRow struct {
	account, family string
	revoked         bool
	rotated         bool
	expires         time.Time
}

// fakeRefresh is an in-memory refresh_token table with the same rules as the real
// store: rotate once, revoke the family on any reuse, slide the expiry on rotation.
// The real queries, including concurrency, are tested in internal/store.
type fakeRefresh struct {
	mu      sync.Mutex
	rows    map[string]*refreshRow // keyed by string(hash)
	revoked [][]byte               // hashes passed to RevokeRefreshFamily
	err     error
}

func newFakeRefresh() *fakeRefresh { return &fakeRefresh{rows: map[string]*refreshRow{}} }

func (f *fakeRefresh) InsertRefreshToken(_ context.Context, account string, hash []byte, expires time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	family := uuid.New().String()
	f.rows[string(hash)] = &refreshRow{account: account, family: family, expires: expires}
	return family, nil
}

func (f *fakeRefresh) RotateRefreshToken(_ context.Context, in store.RotateInput) (store.RotateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.RotateResult{}, f.err
	}
	row, ok := f.rows[string(in.TokenHash)]
	if !ok {
		return store.RotateResult{Outcome: store.RotateInvalid}, nil
	}
	res := store.RotateResult{AccountID: row.account, FamilyID: row.family}
	switch {
	case row.revoked:
		for _, r := range f.rows {
			if r.family == row.family {
				r.revoked = true
			}
		}
		res.Outcome = store.RotateRevoked
		if row.rotated {
			res.Outcome = store.RotateReused
		}
	case !row.expires.After(in.Now):
		res.Outcome = store.RotateInvalid
	default:
		row.revoked, row.rotated = true, true
		f.rows[string(in.NewTokenHash)] = &refreshRow{account: row.account, family: row.family, expires: in.Now.Add(in.TTL)}
		res.Outcome = store.RotateRotated
	}
	return res, nil
}

func (f *fakeRefresh) RevokeRefreshFamily(_ context.Context, hash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.revoked = append(f.revoked, hash)
	if row, ok := f.rows[string(hash)]; ok {
		for _, r := range f.rows {
			if r.family == row.family {
				r.revoked = true
			}
		}
	}
	return nil
}

// lookup finds the row for a raw refresh token as a client holds it.
func (f *fakeRefresh) lookup(token string) (refreshRow, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return refreshRow{}, false
	}
	sum := sha256.Sum256(raw)
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[string(sum[:])]
	if !ok {
		return refreshRow{}, false
	}
	return *row, true
}

func (f *fakeRefresh) families() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, r := range f.rows {
		out[r.family] = true
	}
	return out
}

// sessionFixture serves all three /auth routes over one fake account store and one
// fake refresh store, as the server wires them.
type sessionFixture struct {
	mux      *http.ServeMux
	accounts *fakeAccounts
	refresh  *fakeRefresh
	pub      ed25519.PublicKey
	logs     *bytes.Buffer
	outcomes *recordedOutcomes
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &sessionFixture{accounts: &fakeAccounts{}, refresh: newFakeRefresh(), pub: pub, logs: &bytes.Buffer{}, outcomes: &recordedOutcomes{}}
	log := slog.New(slog.NewJSONHandler(f.logs, nil))
	signer := testSigner(priv)

	f.mux = http.NewServeMux()
	f.mux.Handle("POST /auth/anonymous", api.Anonymous(api.AnonymousDeps{
		Accounts: f.accounts, Refresh: f.refresh, Issuer: signer,
		AccessTTL: 15 * time.Minute, RefreshTTL: 720 * time.Hour,
		Logger: log, Outcomes: f.outcomes,
	}))
	f.mux.Handle("POST /auth/refresh", api.Refresh(api.RefreshDeps{
		Refresh: f.refresh, Issuer: signer,
		AccessTTL: 15 * time.Minute, RefreshTTL: 720 * time.Hour,
		Logger: log, Outcomes: f.outcomes,
	}))
	f.mux.Handle("POST /auth/logout", api.Logout(api.LogoutDeps{Refresh: f.refresh, Logger: log}))
	return f
}

func (f *sessionFixture) post(path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return w
}

// login performs a device login and returns its body.
func (f *sessionFixture) login(t *testing.T) loginBody {
	t.Helper()
	w := f.post("/auth/anonymous", `{"device_id":"`+newDeviceID(t)+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d %s", w.Code, w.Body)
	}
	return decodeLogin(t, w)
}

func (f *sessionFixture) refreshWith(token string) *httptest.ResponseRecorder {
	return f.post("/auth/refresh", `{"refresh_token":"`+token+`"}`)
}

func expectInvalidToken(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("%s: status = %d %s, want 401", what, w.Code, w.Body)
	}
	if code := errorCode(t, w); code != "invalid_token" {
		t.Errorf("%s: code = %q, want invalid_token", what, code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("%s: WWW-Authenticate = %q, want none", what, got)
	}
}

func TestRefreshRotatesAndKeepsTheSubject(t *testing.T) {
	f := newSessionFixture(t)
	login := f.login(t)
	sub := subjectOf(t, f.pub, login.AccessToken)

	w := f.refreshWith(login.RefreshToken)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s, want 200", w.Code, w.Body)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	next := decodeLogin(t, w)
	if next.RefreshToken == "" || next.RefreshToken == login.RefreshToken {
		t.Errorf("refresh returned refresh_token %q, want a new one", next.RefreshToken)
	}
	if got := subjectOf(t, f.pub, next.AccessToken); got != sub {
		t.Errorf("sub after refresh = %q, want %q", got, sub)
	}

	// The successor works in turn: rotation is a chain, not a single step.
	if w := f.refreshWith(next.RefreshToken); w.Code != http.StatusOK {
		t.Errorf("refreshing with the successor = %d %s, want 200", w.Code, w.Body)
	}
}

func TestRefreshReuseRevokesTheWholeFamily(t *testing.T) {
	f := newSessionFixture(t)
	login := f.login(t)
	other := f.login(t) // a separate session that must survive

	next := decodeLogin(t, f.refreshWith(login.RefreshToken))

	// Replaying the spent token is refused and kills the family...
	expectInvalidToken(t, f.refreshWith(login.RefreshToken), "replayed token")
	// ...so the successor the legitimate holder got is dead too.
	expectInvalidToken(t, f.refreshWith(next.RefreshToken), "successor after reuse")

	if !strings.Contains(f.logs.String(), "refresh_token_reuse") {
		t.Error("reuse was not logged")
	}
	for _, tok := range []string{login.RefreshToken, next.RefreshToken} {
		if strings.Contains(f.logs.String(), tok) {
			t.Error("a refresh token was logged")
		}
	}

	if w := f.refreshWith(other.RefreshToken); w.Code != http.StatusOK {
		t.Errorf("an unrelated session was revoked too: %d %s", w.Code, w.Body)
	}
}

func TestRefreshRejectsEveryBadTokenWithOneCode(t *testing.T) {
	f := newSessionFixture(t)
	unknown := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	for name, body := range map[string]string{
		"missing field":      `{}`,
		"null field":         `{"refresh_token":null}`,
		"empty":              `{"refresh_token":""}`,
		"not base64url":      `{"refresh_token":"` + strings.Repeat("!", 43) + `"}`,
		"padded base64":      `{"refresh_token":"` + base64.URLEncoding.EncodeToString(make([]byte, 32)) + `"}`,
		"too short":          `{"refresh_token":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 31)) + `"}`,
		"too long":           `{"refresh_token":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 33)) + `"}`,
		"an access token":    `{"refresh_token":"eyJhbGciOiJFZERTQSJ9.e30.sig"}`, // gitleaks:allow (test fixture)
		"unknown well-shape": `{"refresh_token":"` + unknown + `"}`,
	} {
		expectInvalidToken(t, f.post("/auth/refresh", body), name)
	}
}

func TestRefreshExpiredTokenIsInvalid(t *testing.T) {
	f := newSessionFixture(t)
	login := f.login(t)
	for _, row := range f.refresh.rows {
		row.expires = time.Now().Add(-time.Second)
	}
	expectInvalidToken(t, f.refreshWith(login.RefreshToken), "expired token")
}

func TestRefreshMalformedJSONIsValidationFailed(t *testing.T) {
	f := newSessionFixture(t)
	for name, body := range map[string]string{
		"empty body":       "",
		"not JSON":         "refresh_token=abc",
		"number field":     `{"refresh_token":42}`,
		"trailing garbage": `{"refresh_token":"abc"} x`,
		"over 4 KiB":       `{"refresh_token":"abc"` + strings.Repeat(" ", 5000) + `}`,
	} {
		w := f.post("/auth/refresh", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, w.Code)
			continue
		}
		if code := errorCode(t, w); code != "validation_failed" {
			t.Errorf("%s: code = %q, want validation_failed", name, code)
		}
	}
}

func TestRefreshStoreFailureIsAnOpaque500(t *testing.T) {
	f := newSessionFixture(t)
	login := f.login(t)
	f.refresh.err = errors.New("connection to refresh_token refused")

	w := f.refreshWith(login.RefreshToken)
	if w.Code != http.StatusInternalServerError || errorCode(t, w) != "internal_error" {
		t.Fatalf("refresh with a failing store = %d %s, want 500 internal_error", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "refresh_token refused") {
		t.Errorf("store error leaked: %s", w.Body)
	}
}

func TestLogoutRevokesTheFamilyAndAlwaysAnswers204(t *testing.T) {
	f := newSessionFixture(t)
	login := f.login(t)
	next := decodeLogin(t, f.refreshWith(login.RefreshToken))

	w := f.post("/auth/logout", `{"refresh_token":"`+next.RefreshToken+`"}`)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("logout = %d %q, want 204 with no body", w.Code, w.Body)
	}
	expectInvalidToken(t, f.refreshWith(next.RefreshToken), "refresh after logout")

	// Logout is idempotent, and anything else in the body is still a 204 without a
	// database call.
	calls := len(f.refresh.revoked)
	for name, body := range map[string]string{
		"same token again": `{"refresh_token":"` + next.RefreshToken + `"}`,
		"unknown token":    `{"refresh_token":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 32)) + `"}`,
		"missing field":    `{}`,
		"garbage token":    `{"refresh_token":"nope"}`,
		"malformed JSON":   `{"refresh_token":`,
		"empty body":       "",
	} {
		if w := f.post("/auth/logout", body); w.Code != http.StatusNoContent {
			t.Errorf("%s: logout = %d %s, want 204", name, w.Code, w.Body)
		}
	}
	if got := len(f.refresh.revoked) - calls; got != 2 {
		t.Errorf("the store was asked to revoke %d times, want 2 (only well-formed tokens)", got)
	}
}

func TestLogoutStoreFailureIsA500(t *testing.T) {
	f := newSessionFixture(t)
	login := f.login(t)
	f.refresh.err = errors.New("db down")

	w := f.post("/auth/logout", `{"refresh_token":"`+login.RefreshToken+`"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("logout with a failing store = %d, want 500 (the family may still be live)", w.Code)
	}
}
