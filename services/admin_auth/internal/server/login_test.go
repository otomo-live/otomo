package server_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/admin_auth/internal/config"
	"github.com/otomo-live/otomo/services/admin_auth/internal/password"
	"github.com/otomo-live/otomo/services/admin_auth/internal/server"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
	"github.com/otomo-live/otomo/services/admin_auth/migrations"
)

const (
	loginIssuer   = "https://admin-auth.otomo.internal"
	loginAudience = "otomo:staff"
	loginKid      = "admin-auth-test"
)

// loginConfig is serverTestConfig with the token and lockout settings the login path
// actually reads, so the handler is exercised with production-shaped values.
func loginConfig() config.Config {
	cfg := serverTestConfig()
	cfg.Issuer = loginIssuer
	cfg.Audience = loginAudience
	cfg.AccessTokenTTL = 15 * time.Minute
	cfg.RefreshTokenTTL = 168 * time.Hour
	cfg.LoginMaxFailures = 5
	cfg.LoginLockout = 15 * time.Minute
	return cfg
}

// testDBLockKey mirrors the constant in the store and bootstrap test packages. All
// three share one throwaway database, and the store migration round-trip drops the
// staff tables, so this advisory lock keeps the login tests from running mid-rebuild.
const testDBLockKey int64 = 0x41444D494E // "ADMIN"

// loginDB opens the shared test database and brings it up to the latest embedded
// migration. It skips when ADMIN_AUTH_TEST_DATABASE_URL is unset, like the store tests.
func loginDB(t *testing.T) *store.DB {
	t.Helper()

	url := os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ADMIN_AUTH_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		_ = sqlDB.Close()
		t.Fatalf("sql.Conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, testDBLockKey); err != nil {
		_ = conn.Close()
		_ = sqlDB.Close()
		t.Fatalf("acquire the test database lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, testDBLockKey)
		_ = conn.Close()
		_ = sqlDB.Close()
	})

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.Up(sqlDB, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}

	db, err := store.NewPool(ctx, config.Config{DatabaseURL: url, DBMaxConns: 4})
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// newLoginSigner builds a signer and the JWKS document that publishes its public half.
func newLoginSigner(t *testing.T, cfg config.Config) (*token.Signer, ed25519.PublicKey, []byte) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer := &token.Signer{
		Kid:        loginKid,
		PrivateKey: priv,
		Issuer:     cfg.Issuer,
		Audience:   cfg.Audience,
		TTL:        cfg.AccessTokenTTL,
	}
	raw, err := token.BuildJWKS([]token.PublicKey{{Kid: loginKid, Key: pub}})
	if err != nil {
		t.Fatalf("BuildJWKS: %v", err)
	}
	return signer, pub, raw
}

// uniqueLoginEmail keeps repeat runs against the shared database from colliding.
func uniqueLoginEmail(prefix string) string {
	return fmt.Sprintf("%s-%d@example.com", prefix, time.Now().UnixNano())
}

// insertStaff inserts an account with a real argon2id hash and returns its id.
func insertStaff(t *testing.T, db *store.DB, email, name, pw string, roles []string) string {
	t.Helper()

	hash, err := password.Hash(pw)
	if err != nil {
		t.Fatalf("password.Hash: %v", err)
	}
	var id string
	if err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO staff_user (email, name, password_hash, roles)
		 VALUES ($1, $2, $3, $4) RETURNING id::text`,
		email, name, hash, roles).Scan(&id); err != nil {
		t.Fatalf("insert staff_user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM staff_user WHERE id = $1::uuid`, id)
	})
	return id
}

func postLogin(t *testing.T, base, body string) (int, http.Header, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, base+"/admin-auth/login", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /admin-auth/login: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read login response: %v", err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

// timedLogin is postLogin plus the wall-clock duration of the round trip.
func timedLogin(t *testing.T, base, body string) (int, string, time.Duration) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, base+"/admin-auth/login", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /admin-auth/login: %v", err)
	}
	elapsed := time.Since(start)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read login response: %v", err)
	}
	return resp.StatusCode, string(raw), elapsed
}

// normalizeLoginError strips the per-request request_id so two credential-failure
// bodies can be compared byte for byte.
func normalizeLoginError(t *testing.T, body string) string {
	t.Helper()

	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("body %q is not the COM-5 shape: %v", body, err)
	}
	if e, ok := envelope["error"].(map[string]any); ok {
		delete(e, "request_id")
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("re-marshal body: %v", err)
	}
	return string(out)
}

type loginResponseBody struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	User        struct {
		ID    string   `json:"id"`
		Name  string   `json:"name"`
		Roles []string `json:"roles"`
	} `json:"user"`
}

type apiErrorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func decodeLoginBody(t *testing.T, body string) loginResponseBody {
	t.Helper()
	var got loginResponseBody
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("login body %q is not JSON: %v", body, err)
	}
	return got
}

func decodeAPIError(t *testing.T, body string) apiErrorBody {
	t.Helper()
	var got apiErrorBody
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("error body %q is not the COM-5 shape: %v", body, err)
	}
	return got
}

func TestLoginSuccess(t *testing.T) {
	db := loginDB(t)
	cfg := loginConfig()
	signer, pub, raw := newLoginSigner(t, cfg)
	h := newHarnessWithConfig(t, cfg, server.Deps{JWKS: fakeJWKS{raw: raw}, Signer: signer, Store: db})

	const pw = "s3cret-password"
	email := uniqueLoginEmail("Success")
	id := insertStaff(t, db, email, "Grace Hopper", pw, []string{"viewer"})

	status, header, body := postLogin(t, h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	got := decodeLoginBody(t, body)
	if got.AccessToken == "" {
		t.Error("access_token is empty")
	}
	if got.ExpiresIn != int64(cfg.AccessTokenTTL.Seconds()) {
		t.Errorf("expires_in = %d, want %d", got.ExpiresIn, int64(cfg.AccessTokenTTL.Seconds()))
	}
	if got.User.ID != id || got.User.Name != "Grace Hopper" {
		t.Errorf("user = %+v, want id %s and name Grace Hopper", got.User, id)
	}
	if len(got.User.Roles) != 1 || got.User.Roles[0] != "viewer" {
		t.Errorf("roles = %v, want [viewer]", got.User.Roles)
	}

	// The refresh cookie carries every attribute the contract fixes.
	resp := &http.Response{Header: header}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == testRefreshCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("no %s cookie on a successful login", testRefreshCookieName)
	}
	if !strings.HasPrefix(cookie.Name, "__Host-") {
		t.Errorf("cookie Name = %q, want the __Host- prefix", cookie.Name)
	}
	if cookie.Domain != "" {
		t.Errorf("cookie Domain = %q, want empty (__Host- forbids Domain)", cookie.Domain)
	}
	if cookie.Path != "/" {
		t.Errorf("cookie Path = %q, want /", cookie.Path)
	}
	if cookie.Value == "" {
		t.Error("refresh cookie value is empty")
	}
	if cookie.MaxAge != int(cfg.RefreshTokenTTL.Seconds()) {
		t.Errorf("cookie Max-Age = %d, want %d", cookie.MaxAge, int(cfg.RefreshTokenTTL.Seconds()))
	}
	if !cookie.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if !cookie.Secure {
		t.Error("cookie is not Secure")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite = %v, want Strict", cookie.SameSite)
	}
	setCookie := header.Get("Set-Cookie")
	for _, attr := range []string{"Path=/", "HttpOnly", "Secure", "SameSite=Strict",
		fmt.Sprintf("Max-Age=%d", int(cfg.RefreshTokenTTL.Seconds()))} {
		if !strings.Contains(setCookie, attr) {
			t.Errorf("Set-Cookie %q is missing %q", setCookie, attr)
		}
	}
	if strings.Contains(setCookie, "Domain=") {
		t.Errorf("Set-Cookie %q must not set Domain", setCookie)
	}

	// The access token verifies against the key the service actually publishes.
	jwksStatus, _, jwksBody := get(t, h.public+"/.well-known/jwks.json")
	if jwksStatus != http.StatusOK {
		t.Fatalf("GET jwks = %d, want 200", jwksStatus)
	}
	jwksPub := jwksKey(t, jwksBody)
	if !bytes.Equal(jwksPub, pub) {
		t.Error("the JWKS does not publish the signer's public key")
	}
	parsed, err := jwt.ParseWithClaims(got.AccessToken, &token.Claims{}, func(tk *jwt.Token) (any, error) {
		if tk.Method.Alg() != "EdDSA" {
			return nil, fmt.Errorf("unexpected signing method %s", tk.Method.Alg())
		}
		return jwksPub, nil
	}, jwt.WithIssuer(loginIssuer), jwt.WithAudience(loginAudience), jwt.WithValidMethods([]string{"EdDSA"}))
	if err != nil {
		t.Fatalf("access token does not verify against the JWKS key: %v", err)
	}
	claims := parsed.Claims.(*token.Claims)
	if claims.Subject != id {
		t.Errorf("sub = %q, want %s", claims.Subject, id)
	}
	if claims.Name != "Grace Hopper" {
		t.Errorf("name = %q, want Grace Hopper", claims.Name)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "viewer" {
		t.Errorf("roles = %v, want [viewer]", claims.Roles)
	}

	// The session stores only the sha256 of the cookie value.
	var stored []byte
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT token_hash FROM refresh_session WHERE user_id = $1::uuid ORDER BY created_at DESC LIMIT 1`,
		id).Scan(&stored); err != nil {
		t.Fatalf("read refresh_session: %v", err)
	}
	want := sha256.Sum256([]byte(cookie.Value))
	if !bytes.Equal(stored, want[:]) {
		t.Errorf("stored token_hash = %x, want sha256 of the cookie value", stored)
	}
	if bytes.Equal(stored, []byte(cookie.Value)) {
		t.Error("refresh_session stores the raw cookie value")
	}

	var audited int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'login.success' AND actor_id = $1 AND target = $1`,
		id).Scan(&audited); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audited != 1 {
		t.Errorf("login.success audit rows = %d, want 1", audited)
	}
}

// jwksKey extracts the single Ed25519 public key from a JWKS document.
func jwksKey(t *testing.T, body string) ed25519.PublicKey {
	t.Helper()
	var set struct {
		Keys []struct {
			X string `json:"x"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &set); err != nil {
		t.Fatalf("JWKS %q is not JSON: %v", body, err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("JWKS has %d keys, want 1", len(set.Keys))
	}
	raw, err := base64.RawURLEncoding.DecodeString(set.Keys[0].X)
	if err != nil {
		t.Fatalf("decode JWKS x: %v", err)
	}
	return ed25519.PublicKey(raw)
}

func TestLoginEmailIsCaseInsensitive(t *testing.T) {
	db := loginDB(t)
	cfg := loginConfig()
	signer, _, raw := newLoginSigner(t, cfg)
	h := newHarnessWithConfig(t, cfg, server.Deps{JWKS: fakeJWKS{raw: raw}, Signer: signer, Store: db})

	const pw = "case-password"
	email := uniqueLoginEmail("Case")
	id := insertStaff(t, db, email, "Casey", pw, []string{"viewer"})

	status, _, body := postLogin(t, h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, strings.ToUpper(email), pw))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	if got := decodeLoginBody(t, body); got.User.ID != id {
		t.Errorf("user.id = %q, want %s", got.User.ID, id)
	}
}

func TestLoginFailuresShareOneResponse(t *testing.T) {
	db := loginDB(t)
	cfg := loginConfig()
	signer, _, raw := newLoginSigner(t, cfg)
	h := newHarnessWithConfig(t, cfg, server.Deps{JWKS: fakeJWKS{raw: raw}, Signer: signer, Store: db})

	const pw = "active-password"
	active := uniqueLoginEmail("Active")
	insertStaff(t, db, active, "Active", pw, []string{"viewer"})

	disabled := uniqueLoginEmail("Disabled")
	disabledID := insertStaff(t, db, disabled, "Disabled", pw, []string{"viewer"})
	if _, err := db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET status = 'disabled' WHERE id = $1::uuid`, disabledID); err != nil {
		t.Fatalf("disable staff: %v", err)
	}

	cases := map[string]string{
		"unknown email":  fmt.Sprintf(`{"email":%q,"password":%q}`, uniqueLoginEmail("Ghost"), pw),
		"wrong password": fmt.Sprintf(`{"email":%q,"password":%q}`, active, "not-the-password"),
		"disabled":       fmt.Sprintf(`{"email":%q,"password":%q}`, disabled, pw),
	}

	var first apiErrorBody
	for name, body := range cases {
		status, _, rawBody := postLogin(t, h.public, body)
		if status != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401 (body %s)", name, status, rawBody)
		}
		got := decodeAPIError(t, rawBody)
		if got.Error.Code != "invalid_credentials" {
			t.Errorf("%s: code = %q, want invalid_credentials", name, got.Error.Code)
		}
		if first.Error.Code == "" {
			first = got
			continue
		}
		if got.Error.Code != first.Error.Code || got.Error.Message != first.Error.Message {
			t.Errorf("%s: error = %q/%q, want the same as %q/%q",
				name, got.Error.Code, got.Error.Message, first.Error.Code, first.Error.Message)
		}
	}
}

func TestLoginLockoutAndRecovery(t *testing.T) {
	db := loginDB(t)
	cfg := loginConfig()
	signer, _, raw := newLoginSigner(t, cfg)
	h := newHarnessWithConfig(t, cfg, server.Deps{JWKS: fakeJWKS{raw: raw}, Signer: signer, Store: db})

	const pw = "lockout-password"
	email := uniqueLoginEmail("Lockout")
	id := insertStaff(t, db, email, "Lockout", pw, []string{"viewer"})
	wrong := fmt.Sprintf(`{"email":%q,"password":"wrong"}`, email)
	right := fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw)

	for i := 0; i < 5; i++ {
		status, _, body := postLogin(t, h.public, wrong)
		if status != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d: status = %d, want 401 (body %s)", i+1, status, body)
		}
	}

	var lockedAudits int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'login.locked' AND target = $1`, id).Scan(&lockedAudits); err != nil {
		t.Fatalf("count login.locked audit: %v", err)
	}
	if lockedAudits != 1 {
		t.Errorf("login.locked audit rows = %d, want 1", lockedAudits)
	}

	status, _, body := postLogin(t, h.public, right)
	if status != http.StatusUnauthorized {
		t.Fatalf("correct password while locked: status = %d, want 401 (body %s)", status, body)
	}

	// Move the lock into the past: a non-locked account with the right password logs in.
	if _, err := db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET locked_until = now() - interval '1 minute' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("expire lockout: %v", err)
	}
	status, _, body = postLogin(t, h.public, right)
	if status != http.StatusOK {
		t.Fatalf("after the lock expires: status = %d, want 200 (body %s)", status, body)
	}
}

func TestLoginVerifyRunsExactlyOncePerAttempt(t *testing.T) {
	db := loginDB(t)
	cfg := loginConfig()
	signer, _, raw := newLoginSigner(t, cfg)

	var calls atomic.Int64
	h := newHarnessWithConfig(t, cfg, server.Deps{
		JWKS:   fakeJWKS{raw: raw},
		Signer: signer,
		Store:  db,
		Verify: func(phc, pw string) (bool, error) {
			calls.Add(1)
			return password.Verify(phc, pw)
		},
	})

	// Unknown email still runs the verifier once, against the dummy hash.
	status, _, body := postLogin(t, h.public, fmt.Sprintf(`{"email":%q,"password":"whatever"}`, uniqueLoginEmail("Unknown")))
	if status != http.StatusUnauthorized {
		t.Fatalf("unknown email: status = %d, want 401 (body %s)", status, body)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("verify calls after an unknown email = %d, want 1", got)
	}

	email := uniqueLoginEmail("WrongPw")
	insertStaff(t, db, email, "WrongPw", "the-real-one", []string{"viewer"})
	status, _, body = postLogin(t, h.public, fmt.Sprintf(`{"email":%q,"password":"the-wrong-one"}`, email))
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong password: status = %d, want 401 (body %s)", status, body)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("verify calls after a wrong password = %d, want 2", got)
	}
}

func TestLoginBadBodies(t *testing.T) {
	db := loginDB(t)
	cfg := loginConfig()
	signer, _, raw := newLoginSigner(t, cfg)
	h := newHarnessWithConfig(t, cfg, server.Deps{JWKS: fakeJWKS{raw: raw}, Signer: signer, Store: db})

	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty body", "", "invalid_body"},
		{"not json", "{", "invalid_body"},
		{"unknown field", `{"email":"a@b.c","password":"x","extra":true}`, "invalid_body"},
		{"trailing data", `{"email":"a@b.c","password":"x"} trailing`, "invalid_body"},
		{"two objects", `{"email":"a@b.c","password":"x"}{"email":"a@b.c","password":"x"}`, "invalid_body"},
		{"too large", `{"email":"` + strings.Repeat("a", 20<<10) + `","password":"x"}`, "invalid_body"},
		{"empty email", `{"email":"","password":"x"}`, "validation_failed"},
		{"empty password", `{"email":"a@b.c","password":""}`, "validation_failed"},
		{"missing fields", `{}`, "validation_failed"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, _, body := postLogin(t, h.public, c.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", status, body)
			}
			if got := decodeAPIError(t, body); got.Error.Code != c.code {
				t.Errorf("code = %q, want %q", got.Error.Code, c.code)
			}
		})
	}
}

// A client-controlled User-Agent that is not valid UTF-8, or that the 256-byte cap
// would cut mid-rune, must not break a correct login: refresh_session.user_agent is a
// Postgres text column, which rejects invalid UTF-8.
func TestLoginSurvivesHostileUserAgent(t *testing.T) {
	db := loginDB(t)
	cfg := loginConfig()
	signer, _, raw := newLoginSigner(t, cfg)
	h := newHarnessWithConfig(t, cfg, server.Deps{JWKS: fakeJWKS{raw: raw}, Signer: signer, Store: db})

	const pw = "s3cret-password"
	email := uniqueLoginEmail("HostileUA")
	insertStaff(t, db, email, "UA Test", pw, []string{"viewer"})

	for name, ua := range map[string]string{
		"invalid utf-8":      "curl/8 \xff\xfe",
		"cut through a rune": strings.Repeat("a", 255) + "é",
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, h.public+"/admin-auth/login",
				strings.NewReader(fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("User-Agent", ua)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
			}
		})
	}
}

// An unknown email and a wrong password for a real account must be indistinguishable:
// the same 401 body apart from its request_id, and the same argon2id cost. The timing
// bound is loose (median of 20 runs, 25 ms) and skipped under -short so it does not
// gate fast runs; the body check always runs.
func TestLoginUnknownEmailAndWrongPasswordAreIndistinguishable(t *testing.T) {
	db := loginDB(t)
	cfg := loginConfig()
	signer, _, raw := newLoginSigner(t, cfg)
	h := newHarnessWithConfig(t, cfg, server.Deps{JWKS: fakeJWKS{raw: raw}, Signer: signer, Store: db})

	const pw = "indistinguishable-password"
	email := uniqueLoginEmail("Known")
	insertStaff(t, db, email, "Known User", pw, []string{"viewer"})

	unknownBody := fmt.Sprintf(`{"email":%q,"password":%q}`, uniqueLoginEmail("Unknown"), pw)
	wrongBody := fmt.Sprintf(`{"email":%q,"password":"not-the-password"}`, email)

	unknownStatus, unknownRaw, _ := timedLogin(t, h.public, unknownBody)
	wrongStatus, wrongRaw, _ := timedLogin(t, h.public, wrongBody)
	if unknownStatus != http.StatusUnauthorized || wrongStatus != http.StatusUnauthorized {
		t.Fatalf("statuses = %d/%d, want 401/401", unknownStatus, wrongStatus)
	}
	if got, want := normalizeLoginError(t, unknownRaw), normalizeLoginError(t, wrongRaw); got != want {
		t.Errorf("unknown-email body %s differs from wrong-password body %s", got, want)
	}

	if testing.Short() {
		t.Skip("timing portion skipped under -short")
	}

	// Warm up the HTTP connection and both argon2 paths before sampling.
	_, _, _ = timedLogin(t, h.public, unknownBody)
	_, _, _ = timedLogin(t, h.public, wrongBody)

	median := func(body string) time.Duration {
		const n = 20
		samples := make([]time.Duration, n)
		for i := range samples {
			status, rawBody, d := timedLogin(t, h.public, body)
			if status != http.StatusUnauthorized {
				t.Fatalf("timed login status = %d, want 401 (body %s)", status, rawBody)
			}
			samples[i] = d
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples[n/2]
	}

	unknownMedian := median(unknownBody)
	wrongMedian := median(wrongBody)
	diff := unknownMedian - wrongMedian
	if diff < 0 {
		diff = -diff
	}
	if diff > 25*time.Millisecond {
		t.Errorf("unknown-email median %s vs wrong-password median %s differs by %s, want < 25ms",
			unknownMedian, wrongMedian, diff)
	}
}

// A successful login clears the failure counter, so the next window starts from zero
// instead of locking an account that already proved its password.
func TestLoginSuccessResetsFailureCount(t *testing.T) {
	db := loginDB(t)
	cfg := loginConfig()
	signer, _, raw := newLoginSigner(t, cfg)
	h := newHarnessWithConfig(t, cfg, server.Deps{JWKS: fakeJWKS{raw: raw}, Signer: signer, Store: db})

	const pw = "reset-counter-password"
	email := uniqueLoginEmail("ResetCounter")
	id := insertStaff(t, db, email, "Reset Counter", pw, []string{"viewer"})
	wrong := fmt.Sprintf(`{"email":%q,"password":"wrong"}`, email)
	right := fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw)

	failures := func() int {
		var n int
		if err := db.Pool.QueryRow(context.Background(),
			`SELECT failed_logins FROM staff_user WHERE id = $1::uuid`, id).Scan(&n); err != nil {
			t.Fatalf("read failed_logins: %v", err)
		}
		return n
	}

	// Four failures is one short of the lock; the counter should read 4.
	for i := 0; i < 4; i++ {
		if status, _, body := postLogin(t, h.public, wrong); status != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401 (body %s)", i+1, status, body)
		}
	}
	if got := failures(); got != 4 {
		t.Fatalf("failed_logins after 4 failures = %d, want 4", got)
	}

	// A correct password succeeds and resets the counter to zero.
	if status, _, body := postLogin(t, h.public, right); status != http.StatusOK {
		t.Fatalf("correct password status = %d, want 200 (body %s)", status, body)
	}
	if got := failures(); got != 0 {
		t.Errorf("failed_logins after a successful login = %d, want 0", got)
	}

	// Four more failures are counted from the reset, not from before it, so the
	// account is still one short of the lock and the right password still works.
	for i := 0; i < 4; i++ {
		if status, _, body := postLogin(t, h.public, wrong); status != http.StatusUnauthorized {
			t.Fatalf("post-reset failure %d: status = %d, want 401 (body %s)", i+1, status, body)
		}
	}
	if got := failures(); got != 4 {
		t.Errorf("failed_logins after the reset window = %d, want 4", got)
	}
	if status, _, body := postLogin(t, h.public, right); status != http.StatusOK {
		t.Fatalf("correct password after the reset window = %d, want 200 (body %s)", status, body)
	}
}
