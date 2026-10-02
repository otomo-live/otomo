package server_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/admin_auth/internal/config"
	"github.com/otomo-live/otomo/services/admin_auth/internal/server"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// The refresh cookie's identity, duplicated from internal/api where it is
// unexported. The __Host- prefix is part of the name.
const (
	testRefreshCookieName = "__Host-otomo_refresh"
	testRefreshCookiePath = "/"
)

// sessionEnv is the login harness plus the pieces the refresh, logout and /me tests
// inspect directly: the database, and the key the access tokens are signed with.
type sessionEnv struct {
	h      *harness
	db     *store.DB
	cfg    config.Config
	signer *token.Signer
	pub    ed25519.PublicKey
}

func newSessionEnv(t *testing.T, grace time.Duration) *sessionEnv {
	t.Helper()

	db := loginDB(t)
	cfg := loginConfig()
	cfg.RefreshReuseGrace = grace
	signer, pub, raw := newLoginSigner(t, cfg)
	verifier := token.NewVerifier(cfg.Issuer, cfg.Audience, []token.PublicKey{{Kid: loginKid, Key: pub}})
	h := newHarnessWithConfig(t, cfg, server.Deps{
		JWKS:     fakeJWKS{raw: raw},
		Signer:   signer,
		Verifier: verifier,
		Store:    db,
	})
	return &sessionEnv{h: h, db: db, cfg: cfg, signer: signer, pub: pub}
}

// loginCookie logs in and returns the refresh cookie and the response body.
func loginCookie(t *testing.T, h *harness, email, pw string) (*http.Cookie, loginResponseBody) {
	t.Helper()

	status, header, body := postLogin(t, h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw))
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body %s)", status, body)
	}
	return cookieFromHeader(t, header, testRefreshCookieName), decodeLoginBody(t, body)
}

// cookieFromHeader pulls one cookie out of the Set-Cookie headers.
func cookieFromHeader(t *testing.T, header http.Header, name string) *http.Cookie {
	t.Helper()

	for _, c := range (&http.Response{Header: header}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in %q", name, header.Values("Set-Cookie"))
	return nil
}

// postRefresh calls POST /admin-auth/refresh with the given cookie, or none when it
// is nil.
func postRefresh(t *testing.T, base string, cookie *http.Cookie) (int, http.Header, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, base+"/admin-auth/refresh", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /admin-auth/refresh: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read refresh response: %v", err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

// postLogout calls POST /admin-auth/logout, optionally with a cookie.
func postLogout(t *testing.T, base string, cookie *http.Cookie) (int, http.Header) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, base+"/admin-auth/logout", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /admin-auth/logout: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header
}

// getMe calls GET /admin-auth/me with a bearer token, or none when it is empty.
func getMe(t *testing.T, base, tok string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, base+"/admin-auth/me", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /admin-auth/me: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /me response: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// assertClearedRefreshCookie checks the response expires the refresh cookie.
func assertClearedRefreshCookie(t *testing.T, header http.Header) {
	t.Helper()

	c := cookieFromHeader(t, header, testRefreshCookieName)
	if c.Value != "" {
		t.Errorf("cleared cookie value = %q, want empty", c.Value)
	}
	if c.MaxAge > 0 {
		t.Errorf("cleared cookie Max-Age = %d, want non-positive", c.MaxAge)
	}
	if raw := header.Get("Set-Cookie"); !strings.Contains(raw, "Max-Age=0") {
		t.Errorf("Set-Cookie %q does not expire the cookie with Max-Age=0", raw)
	}
}

// accessClaims verifies a signed access token against the published public key.
func accessClaims(t *testing.T, signed string, pub ed25519.PublicKey) *token.Claims {
	t.Helper()

	parsed, err := jwt.ParseWithClaims(signed, &token.Claims{}, func(tk *jwt.Token) (any, error) {
		if tk.Method.Alg() != "EdDSA" {
			return nil, fmt.Errorf("unexpected signing method %s", tk.Method.Alg())
		}
		return pub, nil
	}, jwt.WithIssuer(loginIssuer), jwt.WithAudience(loginAudience), jwt.WithValidMethods([]string{"EdDSA"}))
	if err != nil {
		t.Fatalf("access token does not verify: %v", err)
	}
	return parsed.Claims.(*token.Claims)
}

func TestRefreshRotates(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "refresh-password"
	email := uniqueLoginEmail("Refresh")
	id := insertStaff(t, env.db, email, "Refresh Test", pw, []string{"viewer"})

	cookie, _ := loginCookie(t, env.h, email, pw)
	oldHash := sha256.Sum256([]byte(cookie.Value))

	status, header, body := postRefresh(t, env.h.public, cookie)
	if status != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 (body %s)", status, body)
	}
	newCookie := cookieFromHeader(t, header, testRefreshCookieName)
	if newCookie.Value == cookie.Value {
		t.Error("refresh returned the same cookie value")
	}
	if newCookie.MaxAge != int(env.cfg.RefreshTokenTTL.Seconds()) {
		t.Errorf("new cookie Max-Age = %d, want %d", newCookie.MaxAge, int(env.cfg.RefreshTokenTTL.Seconds()))
	}
	if newCookie.Path != testRefreshCookiePath || !newCookie.HttpOnly || !newCookie.Secure || newCookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("new cookie = %+v, want the contract attributes", newCookie)
	}

	// The returned access token is valid and describes the same user.
	got := decodeLoginBody(t, body)
	claims := accessClaims(t, got.AccessToken, env.pub)
	if claims.Subject != id {
		t.Errorf("sub = %q, want %s", claims.Subject, id)
	}

	// The old row is stamped rotated and the successor shares its family.
	var (
		oldFamily     string
		oldRotated    *time.Time
		successorFam  string
		successorLive bool
	)
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT family_id::text, rotated_at FROM refresh_session WHERE token_hash = $1`,
		oldHash[:]).Scan(&oldFamily, &oldRotated); err != nil {
		t.Fatalf("read old session: %v", err)
	}
	if oldRotated == nil {
		t.Error("old session rotated_at is NULL after a refresh")
	}

	newHash := sha256.Sum256([]byte(newCookie.Value))
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT family_id::text, (expires_at > now() AND revoked_at IS NULL)
		 FROM refresh_session WHERE token_hash = $1`,
		newHash[:]).Scan(&successorFam, &successorLive); err != nil {
		t.Fatalf("read successor session: %v", err)
	}
	if successorFam != oldFamily {
		t.Errorf("successor family = %s, want %s", successorFam, oldFamily)
	}
	if !successorLive {
		t.Error("successor session is not live")
	}
}

func TestRefreshRereadsRolesFromTheDatabase(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "demotion-password"
	email := uniqueLoginEmail("Demotion")
	id := insertStaff(t, env.db, email, "Demote Me", pw, []string{"viewer", "live_ops"})

	cookie, _ := loginCookie(t, env.h, email, pw)

	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET roles = ARRAY['viewer']::text[] WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("demote staff: %v", err)
	}

	status, _, body := postRefresh(t, env.h.public, cookie)
	if status != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 (body %s)", status, body)
	}
	got := decodeLoginBody(t, body)
	if len(got.User.Roles) != 1 || got.User.Roles[0] != "viewer" {
		t.Errorf("body roles = %v, want [viewer] read after the demotion", got.User.Roles)
	}
	claims := accessClaims(t, got.AccessToken, env.pub)
	if len(claims.Roles) != 1 || claims.Roles[0] != "viewer" {
		t.Errorf("token roles = %v, want [viewer] read after the demotion", claims.Roles)
	}
}

func TestRefreshReuseWithinGrace(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "grace-password"
	email := uniqueLoginEmail("Grace")
	id := insertStaff(t, env.db, email, "Grace Test", pw, []string{"viewer"})

	oldCookie, _ := loginCookie(t, env.h, email, pw)
	status, _, _ := postRefresh(t, env.h.public, oldCookie)
	if status != http.StatusOK {
		t.Fatalf("first refresh status = %d, want 200", status)
	}

	// Presenting the now-rotated old cookie inside the grace window is the benign
	// multi-tab race: success, but no new cookie and no revocation.
	status, header, body := postRefresh(t, env.h.public, oldCookie)
	if status != http.StatusOK {
		t.Fatalf("reuse within grace status = %d, want 200 (body %s)", status, body)
	}
	if set := header.Get("Set-Cookie"); set != "" {
		t.Errorf("reuse within grace set a cookie: %q", set)
	}
	if got := decodeLoginBody(t, body); got.AccessToken == "" {
		t.Error("reuse within grace returned no access token")
	}

	var revoked int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_session WHERE user_id = $1::uuid AND revoked_at IS NOT NULL`, id).Scan(&revoked); err != nil {
		t.Fatalf("count revoked: %v", err)
	}
	if revoked != 0 {
		t.Errorf("revoked sessions after a within-grace reuse = %d, want 0", revoked)
	}

	var audited int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'session.reuse_detected' AND actor_id = $1`, id).Scan(&audited); err != nil {
		t.Fatalf("count reuse audits: %v", err)
	}
	if audited != 0 {
		t.Errorf("reuse audits after a within-grace reuse = %d, want 0", audited)
	}
}

func TestRefreshReuseAfterGraceRevokesTheFamily(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "reuse-password"
	email := uniqueLoginEmail("Reuse")
	id := insertStaff(t, env.db, email, "Reuse Test", pw, []string{"viewer"})

	oldCookie, _ := loginCookie(t, env.h, email, pw)
	status, header, _ := postRefresh(t, env.h.public, oldCookie)
	if status != http.StatusOK {
		t.Fatalf("first refresh status = %d, want 200", status)
	}
	familyID := cookieFamily(t, env.db, oldCookie, header)

	// Push the rotation outside the grace window, as if the token were replayed a
	// minute later.
	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE refresh_session SET rotated_at = now() - interval '1 minute' WHERE family_id = $1::uuid`,
		familyID); err != nil {
		t.Fatalf("age the rotation: %v", err)
	}

	status, header, body := postRefresh(t, env.h.public, oldCookie)
	if status != http.StatusUnauthorized {
		t.Fatalf("reuse after grace status = %d, want 401 (body %s)", status, body)
	}
	if got := decodeAPIError(t, body); got.Error.Code != "invalid_credentials" {
		t.Errorf("code = %q, want invalid_credentials", got.Error.Code)
	}
	assertClearedRefreshCookie(t, header)

	var live int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_session WHERE family_id = $1::uuid AND revoked_at IS NULL`, familyID).Scan(&live); err != nil {
		t.Fatalf("count live family rows: %v", err)
	}
	if live != 0 {
		t.Errorf("live family rows after reuse detection = %d, want 0", live)
	}

	var audits int
	var detailsFamily string
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*), coalesce(max(details->>'family_id'), '')
		 FROM audit_log WHERE action = 'session.reuse_detected' AND actor_id = $1`, id).Scan(&audits, &detailsFamily); err != nil {
		t.Fatalf("read reuse audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("session.reuse_detected rows = %d, want 1", audits)
	}
	if detailsFamily != familyID {
		t.Errorf("reuse audit family_id = %q, want %s", detailsFamily, familyID)
	}
}

func TestRefreshConcurrent(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "concurrent-password"
	email := uniqueLoginEmail("Concurrent")
	id := insertStaff(t, env.db, email, "Concurrent Test", pw, []string{"viewer"})

	cookie, _ := loginCookie(t, env.h, email, pw)

	type result struct {
		status     int
		setCookie  string
		body       string
		reqFailure error
	}
	results := make(chan result, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, env.h.public+"/admin-auth/refresh", nil)
			if err != nil {
				results <- result{reqFailure: err}
				return
			}
			req.AddCookie(cookie)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- result{reqFailure: err}
				return
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				results <- result{reqFailure: err}
				return
			}
			results <- result{status: resp.StatusCode, setCookie: resp.Header.Get("Set-Cookie"), body: string(raw)}
		}()
	}
	wg.Wait()
	close(results)

	rotations := 0
	for res := range results {
		if res.reqFailure != nil {
			t.Fatalf("concurrent refresh: %v", res.reqFailure)
		}
		if res.status != http.StatusOK {
			t.Fatalf("concurrent refresh status = %d, want 200 (body %s)", res.status, res.body)
		}
		if res.setCookie != "" {
			rotations++
		}
	}
	if rotations != 1 {
		t.Errorf("rotations = %d, want exactly 1", rotations)
	}

	var revoked int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_session WHERE user_id = $1::uuid AND revoked_at IS NOT NULL`, id).Scan(&revoked); err != nil {
		t.Fatalf("count revoked: %v", err)
	}
	if revoked != 0 {
		t.Errorf("revoked sessions after concurrent refreshes = %d, want 0", revoked)
	}
}

func TestRefreshRejectsDeadCookies(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)

	cases := []struct {
		name    string
		prepare func(t *testing.T, email, pw string) (*http.Cookie, func(t *testing.T))
	}{
		{
			name: "unknown cookie",
			prepare: func(t *testing.T, email, pw string) (*http.Cookie, func(t *testing.T)) {
				return &http.Cookie{Name: testRefreshCookieName, Value: "no-such-session"}, func(t *testing.T) {}
			},
		},
		{
			name: "expired",
			prepare: func(t *testing.T, email, pw string) (*http.Cookie, func(t *testing.T)) {
				cookie, _ := loginCookie(t, env.h, email, pw)
				return cookie, func(t *testing.T) {
					hash := sha256.Sum256([]byte(cookie.Value))
					if _, err := env.db.Pool.Exec(context.Background(),
						`UPDATE refresh_session SET expires_at = now() - interval '1 hour' WHERE token_hash = $1`, hash[:]); err != nil {
						t.Fatalf("expire session: %v", err)
					}
				}
			},
		},
		{
			name: "revoked",
			prepare: func(t *testing.T, email, pw string) (*http.Cookie, func(t *testing.T)) {
				cookie, _ := loginCookie(t, env.h, email, pw)
				return cookie, func(t *testing.T) {
					hash := sha256.Sum256([]byte(cookie.Value))
					if _, err := env.db.Pool.Exec(context.Background(),
						`UPDATE refresh_session SET revoked_at = now() WHERE token_hash = $1`, hash[:]); err != nil {
						t.Fatalf("revoke session: %v", err)
					}
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const pw = "dead-cookie-password"
			email := uniqueLoginEmail("Dead" + c.name)
			insertStaff(t, env.db, email, "Dead Cookie", pw, []string{"viewer"})

			cookie, afterLogin := c.prepare(t, email, pw)
			afterLogin(t)

			status, header, body := postRefresh(t, env.h.public, cookie)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", status, body)
			}
			if got := decodeAPIError(t, body); got.Error.Code != "invalid_credentials" {
				t.Errorf("code = %q, want invalid_credentials", got.Error.Code)
			}
			assertClearedRefreshCookie(t, header)
		})
	}
}

// TestRefreshIgnoresUnprefixedCookie pins the fix for a cookie bug: only the __Host- form is read,
// so the old unprefixed name a sibling subdomain on the shared parent domain could
// plant is treated exactly like no cookie at all.
func TestRefreshIgnoresUnprefixedCookie(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)

	statusNone, headerNone, bodyNone := postRefresh(t, env.h.public, nil)
	statusOld, headerOld, bodyOld := postRefresh(t, env.h.public, &http.Cookie{
		Name:  "otomo_refresh",
		Value: "planted-by-a-sibling-subdomain",
	})

	if statusNone != http.StatusUnauthorized || statusOld != http.StatusUnauthorized {
		t.Fatalf("refresh status = no cookie %d, old name %d; want both 401", statusNone, statusOld)
	}
	if got, want := normalizeLoginError(t, bodyOld), normalizeLoginError(t, bodyNone); got != want {
		t.Errorf("old-name cookie body = %s, want the no-cookie body %s", got, want)
	}
	// Both refusals clear the __Host- cookie the browser should hold.
	assertClearedRefreshCookie(t, headerNone)
	assertClearedRefreshCookie(t, headerOld)
}

func TestRefreshDisabledUserRevokesFamily(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "disabled-refresh-password"
	email := uniqueLoginEmail("DisabledRefresh")
	id := insertStaff(t, env.db, email, "Disabled Refresh", pw, []string{"viewer"})

	cookie, _ := loginCookie(t, env.h, email, pw)
	familyID := singleFamily(t, env.db, id)

	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET status = 'disabled' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("disable staff: %v", err)
	}

	status, header, body := postRefresh(t, env.h.public, cookie)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", status, body)
	}
	assertClearedRefreshCookie(t, header)

	var live int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_session WHERE family_id = $1::uuid AND revoked_at IS NULL`, familyID).Scan(&live); err != nil {
		t.Fatalf("count live family rows: %v", err)
	}
	if live != 0 {
		t.Errorf("live family rows after disabling the user = %d, want 0", live)
	}
}

func TestLogoutRevokesFamilyAndAlwaysClears(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "logout-password"
	email := uniqueLoginEmail("Logout")
	id := insertStaff(t, env.db, email, "Logout Test", pw, []string{"viewer"})

	cookie, _ := loginCookie(t, env.h, email, pw)
	familyID := singleFamily(t, env.db, id)

	status, header := postLogout(t, env.h.public, cookie)
	if status != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", status)
	}
	assertClearedRefreshCookie(t, header)

	var live int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_session WHERE family_id = $1::uuid AND revoked_at IS NULL`, familyID).Scan(&live); err != nil {
		t.Fatalf("count live family rows: %v", err)
	}
	if live != 0 {
		t.Errorf("live family rows after logout = %d, want 0", live)
	}

	var audits int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'logout' AND actor_id = $1`, id).Scan(&audits); err != nil {
		t.Fatalf("count logout audits: %v", err)
	}
	if audits != 1 {
		t.Errorf("logout audits = %d, want 1", audits)
	}

	// The revoked cookie can no longer refresh.
	status, _, _ = postRefresh(t, env.h.public, cookie)
	if status != http.StatusUnauthorized {
		t.Errorf("refresh after logout = %d, want 401", status)
	}

	// Logout without a cookie is still a 204.
	status, header = postLogout(t, env.h.public, nil)
	if status != http.StatusNoContent {
		t.Errorf("logout without a cookie = %d, want 204", status)
	}
	assertClearedRefreshCookie(t, header)
}

func TestMeReadsTheUserFromTheDatabase(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "me-password"
	email := uniqueLoginEmail("Me")
	id := insertStaff(t, env.db, email, "Original Name", pw, []string{"viewer", "live_ops"})

	_, body := loginCookie(t, env.h, email, pw)
	access := body.AccessToken

	// Change the row behind the token's back: /me must report the DB, not the claims.
	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET name = 'Renamed', roles = ARRAY['viewer']::text[] WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("update staff: %v", err)
	}

	status, raw := getMe(t, env.h.public, access)
	if status != http.StatusOK {
		t.Fatalf("me status = %d, want 200 (body %s)", status, raw)
	}
	var got struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Roles      []string `json:"roles"`
		IsRoot     *bool    `json:"is_root"`
		MFAEnabled *bool    `json:"mfa_enabled"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("me body %q is not JSON: %v", raw, err)
	}
	if got.ID != id || got.Name != "Renamed" {
		t.Errorf("me = %+v, want id %s and the renamed user", got, id)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "viewer" {
		t.Errorf("me roles = %v, want [viewer] from the database", got.Roles)
	}
	if got.IsRoot == nil || *got.IsRoot || got.MFAEnabled == nil || *got.MFAEnabled {
		t.Errorf("me = %s, want is_root and mfa_enabled present and false", raw)
	}

	// A confirmed factor shows up on the next call, again read from the row.
	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET totp_confirmed_at = now() WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("confirm totp: %v", err)
	}
	_, raw = getMe(t, env.h.public, access)
	got.MFAEnabled = nil
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("me body %q is not JSON: %v", raw, err)
	}
	if got.MFAEnabled == nil || !*got.MFAEnabled {
		t.Errorf("me = %s, want mfa_enabled true after confirmation", raw)
	}
}

func TestMeRejections(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "me-reject-password"
	email := uniqueLoginEmail("MeReject")
	id := insertStaff(t, env.db, email, "Me Reject", pw, []string{"viewer"})

	_, body := loginCookie(t, env.h, email, pw)
	valid := body.AccessToken
	tampered := tamperSignature(t, valid)

	expiredSigner := *env.signer
	expiredSigner.TTL = -time.Hour
	expired, err := expiredSigner.Issue(id, "Me Reject", []string{"viewer"}, time.Now())
	if err != nil {
		t.Fatalf("Issue expired: %v", err)
	}

	wrongAud := *env.signer
	wrongAud.Audience = "otomo:player"
	wrongAudience, err := wrongAud.Issue(id, "Me Reject", []string{"viewer"}, time.Now())
	if err != nil {
		t.Fatalf("Issue wrong aud: %v", err)
	}

	unknownKid := *env.signer
	unknownKid.Kid = "admin-auth-unknown"
	unknownKidToken, err := unknownKid.Issue(id, "Me Reject", []string{"viewer"}, time.Now())
	if err != nil {
		t.Fatalf("Issue unknown kid: %v", err)
	}

	cases := []struct {
		name   string
		token  string
		reason string
	}{
		{"missing", "", "missing_token"},
		{"tampered", tampered, "invalid_signature"},
		{"expired", expired, "expired"},
		{"wrong audience", wrongAudience, "aud_mismatch"},
		{"unknown kid", unknownKidToken, "invalid_signature"},
		{"garbage", "not.a.jwt", "invalid_token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := getMe(t, env.h.public, c.token)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", status, raw)
			}
			if got := decodeAPIError(t, raw); got.Error.Code != c.reason {
				t.Errorf("code = %q, want %q", got.Error.Code, c.reason)
			}
		})
	}

	// A disabled account stops resolving even though its token is still valid.
	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET status = 'disabled' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("disable staff: %v", err)
	}
	status, raw := getMe(t, env.h.public, valid)
	if status != http.StatusUnauthorized {
		t.Fatalf("disabled user me status = %d, want 401 (body %s)", status, raw)
	}
	if got := decodeAPIError(t, raw); got.Error.Code != "invalid_token" {
		t.Errorf("disabled user code = %q, want invalid_token", got.Error.Code)
	}
}

// tamperSignature flips one significant character in the JWT's signature segment.
// The last character of a base64url signature may carry only padding bits, so
// tampering there can decode to the same signature; the middle of the segment cannot.
func tamperSignature(t *testing.T, signed string) string {
	t.Helper()

	dot := strings.LastIndex(signed, ".")
	if dot < 0 || dot+10 >= len(signed) {
		t.Fatalf("token %q has no signature segment", signed)
	}
	i := dot + 10
	replacement := byte('A')
	if signed[i] == 'A' {
		replacement = 'B'
	}
	return signed[:i] + string(replacement) + signed[i+1:]
}

// cookieFamily resolves the family a cookie's session belongs to.
func cookieFamily(t *testing.T, db *store.DB, cookie *http.Cookie, header http.Header) string {
	t.Helper()

	hash := sha256.Sum256([]byte(cookie.Value))
	var family string
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT family_id::text FROM refresh_session WHERE token_hash = $1`, hash[:]).Scan(&family); err != nil {
		t.Fatalf("resolve cookie family: %v", err)
	}
	return family
}

// singleFamily returns the only family a freshly logged-in user has.
func singleFamily(t *testing.T, db *store.DB, userID string) string {
	t.Helper()

	var family string
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT family_id::text FROM refresh_session WHERE user_id = $1::uuid ORDER BY created_at DESC LIMIT 1`,
		userID).Scan(&family); err != nil {
		t.Fatalf("resolve user family: %v", err)
	}
	return family
}

// Disabling an account must win over the multi-tab grace window: replaying the
// just-rotated cookie right after the account was disabled gets no access token.
func TestRefreshGraceDoesNotResurrectADisabledUser(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "grace-disabled-password"
	email := uniqueLoginEmail("GraceDisabled")
	id := insertStaff(t, env.db, email, "Grace Disabled", pw, []string{"viewer"})

	oldCookie, _ := loginCookie(t, env.h, email, pw)
	if status, _, _ := postRefresh(t, env.h.public, oldCookie); status != http.StatusOK {
		t.Fatalf("first refresh status = %d, want 200", status)
	}
	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET status = 'disabled' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("disable staff: %v", err)
	}

	status, header, body := postRefresh(t, env.h.public, oldCookie)
	if status != http.StatusUnauthorized {
		t.Fatalf("grace replay for a disabled user = %d, want 401 (body %s)", status, body)
	}
	assertClearedRefreshCookie(t, header)
}

// refreshAttempt is one result of a concurrent refresh burst.
type refreshAttempt struct {
	status    int
	setCookie string
	body      string
	err       error
}

// concurrentRefresh fires n simultaneous POST /admin-auth/refresh requests that all
// present the same cookie, and returns each attempt's result. Each goroutine writes a
// distinct slice slot, so no lock is needed.
func concurrentRefresh(t *testing.T, base string, cookie *http.Cookie, n int) []refreshAttempt {
	t.Helper()

	results := make([]refreshAttempt, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, base+"/admin-auth/refresh", nil)
			if err != nil {
				results[i] = refreshAttempt{err: err}
				return
			}
			req.AddCookie(cookie)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results[i] = refreshAttempt{err: err}
				return
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				results[i] = refreshAttempt{err: err}
				return
			}
			results[i] = refreshAttempt{
				status:    resp.StatusCode,
				setCookie: resp.Header.Get("Set-Cookie"),
				body:      string(raw),
			}
		}(i)
	}
	wg.Wait()
	return results
}

// N=10 simultaneous refreshes of one cookie must serialise on the session row: exactly
// one rotates and sends a fresh cookie, and the other nine take the grace path (200,
// no Set-Cookie). The family ends with the old rotated row plus one live successor.
func TestRefreshConcurrentExactlyOneRotation(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "concurrent-rotation-password"
	email := uniqueLoginEmail("ConcurrentRotation")
	id := insertStaff(t, env.db, email, "Concurrent Rotation", pw, []string{"viewer"})

	cookie, _ := loginCookie(t, env.h, email, pw)
	familyID := singleFamily(t, env.db, id)

	const n = 10
	results := concurrentRefresh(t, env.h.public, cookie, n)

	rotations, grace := 0, 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("attempt %d: %v", i, r.err)
		}
		if r.status != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want 200 (body %s)", i, r.status, r.body)
		}
		if r.setCookie == "" {
			grace++
			continue
		}
		rotations++
		if strings.Contains(r.setCookie, cookie.Value) {
			t.Errorf("attempt %d returned a cookie with the old value", i)
		}
	}
	if rotations != 1 {
		t.Errorf("rotations (Set-Cookie with a new value) = %d, want exactly 1", rotations)
	}
	if grace != n-1 {
		t.Errorf("grace-path successes (no Set-Cookie) = %d, want %d", grace, n-1)
	}

	var rows, rotated, live int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*),
		        count(*) FILTER (WHERE rotated_at IS NOT NULL),
		        count(*) FILTER (WHERE rotated_at IS NULL AND revoked_at IS NULL AND expires_at > now())
		 FROM refresh_session WHERE family_id = $1::uuid`,
		familyID).Scan(&rows, &rotated, &live); err != nil {
		t.Fatalf("count family rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("family rows = %d, want 2 (the rotated token plus one successor)", rows)
	}
	if rotated != 1 {
		t.Errorf("rotated rows = %d, want 1", rotated)
	}
	if live != 1 {
		t.Errorf("live successors = %d, want 1", live)
	}

	var revoked int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_session WHERE family_id = $1::uuid AND revoked_at IS NOT NULL`,
		familyID).Scan(&revoked); err != nil {
		t.Fatalf("count revoked: %v", err)
	}
	if revoked != 0 {
		t.Errorf("revoked rows after a within-grace burst = %d, want 0", revoked)
	}
}

// With the benign-reuse window disabled, the loser of the rotation race is a replay:
// exactly one request succeeds and rotates, every other gets 401.
func TestRefreshGraceDisabledAllowsExactlyOneRotation(t *testing.T) {
	env := newSessionEnv(t, 0)
	const pw = "no-grace-password"
	email := uniqueLoginEmail("NoGrace")
	id := insertStaff(t, env.db, email, "No Grace", pw, []string{"viewer"})

	cookie, _ := loginCookie(t, env.h, email, pw)
	familyID := singleFamily(t, env.db, id)

	const n = 10
	results := concurrentRefresh(t, env.h.public, cookie, n)

	ok, unauthorized, rotations := 0, 0, 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("attempt %d: %v", i, r.err)
		}
		switch r.status {
		case http.StatusOK:
			ok++
			if r.setCookie != "" {
				rotations++
			}
		case http.StatusUnauthorized:
			unauthorized++
			if got := decodeAPIError(t, r.body).Error.Code; got != "invalid_credentials" {
				t.Errorf("attempt %d code = %q, want invalid_credentials", i, got)
			}
		default:
			t.Fatalf("attempt %d status = %d, want 200 or 401 (body %s)", i, r.status, r.body)
		}
	}
	if ok != 1 {
		t.Errorf("200 responses = %d, want exactly 1", ok)
	}
	if unauthorized != n-1 {
		t.Errorf("401 responses = %d, want %d", unauthorized, n-1)
	}
	if rotations != 1 {
		t.Errorf("rotations = %d, want exactly 1", rotations)
	}

	var rows int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_session WHERE family_id = $1::uuid`, familyID).Scan(&rows); err != nil {
		t.Fatalf("count family rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("family rows = %d, want 2 (one successor only)", rows)
	}
}
