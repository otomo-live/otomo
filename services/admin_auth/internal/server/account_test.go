package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/admin_auth/internal/mfa"
)

// The account endpoints' wire shapes, duplicated from internal/api where they are
// unexported.
type accountSessionWire struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	IP         *string   `json:"ip"`
	UserAgent  *string   `json:"user_agent"`
	Current    bool      `json:"current"`
}

type accountSessionsWire struct {
	Sessions []accountSessionWire `json:"sessions"`
}

type revokeOthersWire struct {
	Revoked     int  `json:"revoked"`
	CurrentKept bool `json:"current_kept"`
}

type recoveryCodesWire struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

// accountReq sends one account request with an optional bearer and cookie.
func accountReq(t *testing.T, method, base, path, tok string, cookie *http.Cookie, body string) (int, http.Header, string) {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

// issueAccountToken mints an access token for an account the test created directly,
// so a confirmed-factor account does not have to walk the login challenge.
func (e *mfaEnv) issueAccountToken(t *testing.T, id, name string, roles []string) string {
	t.Helper()

	tok, err := e.signer.Issue(id, name, roles, e.now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

func decodeSessions(t *testing.T, body string) accountSessionsWire {
	t.Helper()

	var got accountSessionsWire
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("sessions body %q is not JSON: %v", body, err)
	}
	return got
}

func TestAccountPasswordChange(t *testing.T) {
	env := newMFAEnv(t)
	id, email, pw, _ := env.staff(t, []string{"viewer"}, false)
	token := env.issueAccountToken(t, id, "MFA Staff", []string{"viewer"})

	cookieA, _ := loginCookie(t, env.h, email, pw)
	cookieB, _ := loginCookie(t, env.h, email, pw)

	// Wrong current password is the login 401, and the account is otherwise untouched.
	status, _, body := accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/password", token, cookieA,
		fmt.Sprintf(`{"current_password":"not-it","new_password":"brand-new-account-password"}`))
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong current status = %d, want 401 (body %s)", status, body)
	}
	if got := decodeAPIError(t, body).Error.Code; got != "invalid_credentials" {
		t.Errorf("wrong current code = %q, want invalid_credentials", got)
	}

	// A weak new password is refused by name, once the current one is right.
	status, _, body = accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/password", token, cookieA,
		fmt.Sprintf(`{"current_password":%q,"new_password":"short"}`, pw))
	if status != http.StatusBadRequest {
		t.Fatalf("weak new status = %d, want 400 (body %s)", status, body)
	}
	if got := decodeAPIError(t, body).Error.Code; got != "validation_failed" {
		t.Errorf("weak new code = %q, want validation_failed", got)
	}

	// Reusing the current password is refused, as is too.
	status, _, body = accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/password", token, cookieA,
		fmt.Sprintf(`{"current_password":%q,"new_password":%q}`, pw, pw))
	if status != http.StatusBadRequest {
		t.Fatalf("same-as-current status = %d, want 400 (body %s)", status, body)
	}
	if got := decodeAPIError(t, body).Error.Code; got != "validation_failed" {
		t.Errorf("same-as-current code = %q, want validation_failed", got)
	}

	const newPW = "brand-new-account-password"
	status, _, body = accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/password", token, cookieA,
		fmt.Sprintf(`{"current_password":%q,"new_password":%q}`, pw, newPW))
	if status != http.StatusNoContent {
		t.Fatalf("change status = %d, want 204 (body %s)", status, body)
	}

	// The old password no longer logs in; the new one does.
	if status, _, _ := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw)); status != http.StatusUnauthorized {
		t.Errorf("old password login = %d, want 401", status)
	}
	if status, _, _ := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, newPW)); status != http.StatusOK {
		t.Errorf("new password login = %d, want 200", status)
	}

	// Every other session is revoked; the caller's own cookie still refreshes.
	if status, _, _ := postRefresh(t, env.h.public, cookieB); status != http.StatusUnauthorized {
		t.Errorf("other session refresh = %d, want 401", status)
	}
	if status, _, body := postRefresh(t, env.h.public, cookieA); status != http.StatusOK {
		t.Errorf("current session refresh = %d, want 200 (body %s)", status, body)
	}

	var audits int
	var revoked int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*), coalesce(max((details->>'sessions_revoked')::int), 0)
		   FROM audit_log WHERE action = 'account.password_changed' AND actor_id = $1`, id).Scan(&audits, &revoked); err != nil {
		t.Fatalf("read password audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("account.password_changed rows = %d, want 1", audits)
	}
	if revoked != 1 {
		t.Errorf("sessions_revoked = %d, want 1", revoked)
	}
}

func TestAccountPasswordChangeLockout(t *testing.T) {
	env := newMFAEnv(t)
	id, _, pw, _ := env.staff(t, []string{"viewer"}, false)
	token := env.issueAccountToken(t, id, "MFA Staff", []string{"viewer"})

	for i := 0; i < env.cfg.LoginMaxFailures; i++ {
		status, _, body := accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/password", token, nil,
			fmt.Sprintf(`{"current_password":"wrong-%d","new_password":"brand-new-account-password"}`, i))
		if status != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401 (body %s)", i, status, body)
		}
	}

	// The account is locked: even the right current password is refused.
	status, _, body := accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/password", token, nil,
		fmt.Sprintf(`{"current_password":%q,"new_password":"brand-new-account-password"}`, pw))
	if status != http.StatusUnauthorized {
		t.Fatalf("locked correct-password status = %d, want 401 (body %s)", status, body)
	}
	if got := decodeAPIError(t, body).Error.Code; got != "invalid_credentials" {
		t.Errorf("locked code = %q, want invalid_credentials", got)
	}
}

func TestAccountSessionsListAndRevoke(t *testing.T) {
	env := newMFAEnv(t)
	id, email, pw, _ := env.staff(t, []string{"viewer"}, false)
	token := env.issueAccountToken(t, id, "MFA Staff", []string{"viewer"})

	cookieA, _ := loginCookie(t, env.h, email, pw)
	cookieB, _ := loginCookie(t, env.h, email, pw)

	status, _, body := accountReq(t, http.MethodGet, env.h.public, "/admin-auth/account/sessions", token, cookieA, "")
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", status, body)
	}
	got := decodeSessions(t, body)
	if len(got.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2 (body %s)", len(got.Sessions), body)
	}
	// Newest first: B logged in after A, so B is first and neither is current for
	// cookieA's request except A itself.
	if !got.Sessions[0].LastUsedAt.After(got.Sessions[1].LastUsedAt) {
		t.Errorf("sessions are not newest-first: %+v", got.Sessions)
	}
	current := 0
	for _, s := range got.Sessions {
		if s.Current {
			current++
			wantID := sessionIDForCookie(t, env, cookieA)
			if s.ID != wantID {
				t.Errorf("current session id = %s, want %s", s.ID, wantID)
			}
		}
	}
	if current != 1 {
		t.Errorf("current sessions = %d, want exactly 1", current)
	}

	status, _, body = accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/sessions/revoke-others", token, cookieA, "")
	if status != http.StatusOK {
		t.Fatalf("revoke-others status = %d, want 200 (body %s)", status, body)
	}
	var revoke revokeOthersWire
	if err := json.Unmarshal([]byte(body), &revoke); err != nil {
		t.Fatalf("revoke-others body %q is not JSON: %v", body, err)
	}
	if revoke.Revoked != 1 || !revoke.CurrentKept {
		t.Errorf("revoke-others = %+v, want revoked 1 and current_kept true", revoke)
	}

	if status, _, _ := postRefresh(t, env.h.public, cookieB); status != http.StatusUnauthorized {
		t.Errorf("revoked other session refresh = %d, want 401", status)
	}
	if status, _, body := postRefresh(t, env.h.public, cookieA); status != http.StatusOK {
		t.Errorf("current session refresh = %d, want 200 (body %s)", status, body)
	}

	// Without a cookie the route revokes everything and says so.
	status, _, body = accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/sessions/revoke-others", token, nil, "")
	if status != http.StatusOK {
		t.Fatalf("revoke-others without cookie status = %d, want 200 (body %s)", status, body)
	}
	if err := json.Unmarshal([]byte(body), &revoke); err != nil {
		t.Fatalf("revoke-others body %q is not JSON: %v", body, err)
	}
	if revoke.CurrentKept {
		t.Errorf("current_kept = true without a cookie, want false (body %s)", body)
	}
	if revoke.Revoked < 1 {
		t.Errorf("revoked = %d without a cookie, want at least 1", revoke.Revoked)
	}
	if status, _, _ := postRefresh(t, env.h.public, cookieA); status != http.StatusUnauthorized {
		t.Errorf("refresh after revoke-all = %d, want 401", status)
	}
}

// sessionIDForCookie resolves the refresh_session row a cookie's hash points at.
func sessionIDForCookie(t *testing.T, env *mfaEnv, cookie *http.Cookie) string {
	t.Helper()

	hash := sha256.Sum256([]byte(cookie.Value))
	var id string
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT id::text FROM refresh_session WHERE token_hash = $1`, hash[:]).Scan(&id); err != nil {
		t.Fatalf("resolve session id: %v", err)
	}
	return id
}

func TestAccountRecoveryCodesRegenerate(t *testing.T) {
	env := newMFAEnv(t)
	id, email, pw, secret := env.staff(t, []string{"live_ops"}, true)
	token := env.issueAccountToken(t, id, "MFA Staff", []string{"live_ops"})

	oldCodes := []string{"aaaa-aaaa", "bbbb-bbbb"}
	for _, code := range oldCodes {
		if _, err := env.db.Pool.Exec(context.Background(),
			`INSERT INTO mfa_recovery_code (user_id, code_hash) VALUES ($1::uuid, $2)`,
			id, mfa.HashRecoveryCode(code)); err != nil {
			t.Fatalf("insert old recovery code: %v", err)
		}
	}

	code := env.code(secret)
	status, _, body := accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/mfa/recovery-codes", token, nil,
		fmt.Sprintf(`{"code":%q}`, code))
	if status != http.StatusOK {
		t.Fatalf("recovery regen status = %d, want 200 (body %s)", status, body)
	}
	var got recoveryCodesWire
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("recovery body %q is not JSON: %v", body, err)
	}
	if len(got.RecoveryCodes) != 10 {
		t.Fatalf("recovery_codes = %d, want 10", len(got.RecoveryCodes))
	}
	seen := map[string]bool{}
	for _, c := range got.RecoveryCodes {
		if len(c) != 9 || seen[c] {
			t.Errorf("recovery code %q is malformed or duplicated", c)
		}
		seen[c] = true
	}

	// The old codes are gone and the new hashes are stored.
	for _, old := range oldCodes {
		var n int
		if err := env.db.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM mfa_recovery_code WHERE user_id = $1::uuid AND code_hash = $2`,
			id, mfa.HashRecoveryCode(old)).Scan(&n); err != nil {
			t.Fatalf("count old code: %v", err)
		}
		if n != 0 {
			t.Errorf("old recovery code %q is still stored", old)
		}
	}
	var stored int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mfa_recovery_code WHERE user_id = $1::uuid`, id).Scan(&stored); err != nil {
		t.Fatalf("count recovery codes: %v", err)
	}
	if stored != 10 {
		t.Errorf("stored recovery codes = %d, want 10", stored)
	}

	// The TOTP step was consumed: replaying the same code is refused.
	status, _, body = accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/mfa/recovery-codes", token, nil,
		fmt.Sprintf(`{"code":%q}`, code))
	if status != http.StatusUnauthorized {
		t.Fatalf("replayed totp status = %d, want 401 (body %s)", status, body)
	}
	if c := decodeAPIError(t, body).Error.Code; c != "invalid_code" {
		t.Errorf("replayed totp code = %q, want invalid_code", c)
	}

	// An old code no longer finishes a login challenge.
	ticket := loginTicket(t, env, email, pw)
	status, _, body = postJSON(t, env.h.public, "/admin-auth/mfa/verify", "",
		fmt.Sprintf(`{"mfa_ticket":%q,"code":%q}`, ticket.MFATicket, oldCodes[0]))
	if status != http.StatusUnauthorized {
		t.Fatalf("old recovery code verify = %d, want 401 (body %s)", status, body)
	}

	// Each new code works exactly once.
	for i, c := range got.RecoveryCodes {
		ticket := loginTicket(t, env, email, pw)
		status, _, body = postJSON(t, env.h.public, "/admin-auth/mfa/verify", "",
			fmt.Sprintf(`{"mfa_ticket":%q,"code":%q}`, ticket.MFATicket, c))
		if status != http.StatusOK {
			t.Fatalf("recovery code %d (%s) status = %d, want 200 (body %s)", i, c, status, body)
		}
	}
	ticket = loginTicket(t, env, email, pw)
	status, _, body = postJSON(t, env.h.public, "/admin-auth/mfa/verify", "",
		fmt.Sprintf(`{"mfa_ticket":%q,"code":%q}`, ticket.MFATicket, got.RecoveryCodes[0]))
	if status != http.StatusUnauthorized {
		t.Fatalf("reused recovery code status = %d, want 401 (body %s)", status, body)
	}

	var audits int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'mfa.recovery_regenerated' AND actor_id = $1`, id).Scan(&audits); err != nil {
		t.Fatalf("count recovery audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("mfa.recovery_regenerated rows = %d, want 1", audits)
	}
}

func TestAccountDisableMFA(t *testing.T) {
	t.Run("live_ops with TOTP disables and login stops challenging", func(t *testing.T) {
		env := newMFAEnv(t)
		id, email, pw, secret := env.staff(t, []string{"live_ops"}, true)
		token := env.issueAccountToken(t, id, "MFA Staff", []string{"live_ops"})

		status, _, body := accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/mfa/disable", token, nil,
			fmt.Sprintf(`{"code":%q}`, env.code(secret)))
		if status != http.StatusNoContent {
			t.Fatalf("disable status = %d, want 204 (body %s)", status, body)
		}

		var confirmed *time.Time
		var secretEnc []byte
		var step *int64
		var codes int
		if err := env.db.Pool.QueryRow(context.Background(),
			`SELECT totp_confirmed_at, totp_secret_enc, totp_last_step FROM staff_user WHERE id = $1::uuid`, id).
			Scan(&confirmed, &secretEnc, &step); err != nil {
			t.Fatalf("read cleared factor: %v", err)
		}
		if confirmed != nil || secretEnc != nil || step != nil {
			t.Errorf("factor not fully cleared: confirmed=%v secret=%v step=%v", confirmed, secretEnc, step)
		}
		if err := env.db.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM mfa_recovery_code WHERE user_id = $1::uuid`, id).Scan(&codes); err != nil {
			t.Fatalf("count recovery codes: %v", err)
		}
		if codes != 0 {
			t.Errorf("recovery codes after disable = %d, want 0", codes)
		}

		var audits int
		if err := env.db.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_log WHERE action = 'mfa.disabled' AND actor_id = $1`, id).Scan(&audits); err != nil {
			t.Fatalf("count disable audit: %v", err)
		}
		if audits != 1 {
			t.Errorf("mfa.disabled rows = %d, want 1", audits)
		}

		// Login now issues tokens without an MFA challenge.
		status, _, body = postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw))
		if status != http.StatusOK {
			t.Fatalf("login after disable = %d, want 200 (body %s)", status, body)
		}
		if got := decodeLoginBody(t, body); got.AccessToken == "" {
			t.Error("login after disable returned no access token")
		}
	})

	t.Run("recovery code is accepted", func(t *testing.T) {
		env := newMFAEnv(t)
		id, _, _, _ := env.staff(t, []string{"viewer"}, true)
		token := env.issueAccountToken(t, id, "MFA Staff", []string{"viewer"})
		const recovery = "cccc-cccc"
		if _, err := env.db.Pool.Exec(context.Background(),
			`INSERT INTO mfa_recovery_code (user_id, code_hash) VALUES ($1::uuid, $2)`,
			id, mfa.HashRecoveryCode(recovery)); err != nil {
			t.Fatalf("insert recovery code: %v", err)
		}

		status, _, body := accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/mfa/disable", token, nil,
			fmt.Sprintf(`{"code":%q}`, recovery))
		if status != http.StatusNoContent {
			t.Fatalf("disable with recovery code status = %d, want 204 (body %s)", status, body)
		}
	})

	t.Run("admin is refused", func(t *testing.T) {
		env := newMFAEnv(t)
		id, _, _, secret := env.staff(t, []string{"admin"}, true)
		token := env.issueAccountToken(t, id, "MFA Staff", []string{"admin"})

		status, _, body := accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/mfa/disable", token, nil,
			fmt.Sprintf(`{"code":%q}`, env.code(secret)))
		if status != http.StatusForbidden {
			t.Fatalf("admin disable status = %d, want 403 (body %s)", status, body)
		}
		if c := decodeAPIError(t, body).Error.Code; c != "mfa_required" {
			t.Errorf("admin disable code = %q, want mfa_required", c)
		}
	})

	t.Run("root is refused", func(t *testing.T) {
		env := newMFAEnv(t)
		id, _ := env.insertRoot(t, "root-disable-password")
		token := env.issueAccountToken(t, id, "Root MFA", []string{"admin"})

		status, _, body := accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/mfa/disable", token, nil,
			`{"code":"123456"}`)
		if status != http.StatusForbidden {
			t.Fatalf("root disable status = %d, want 403 (body %s)", status, body)
		}
		if c := decodeAPIError(t, body).Error.Code; c != "root_protected" {
			t.Errorf("root disable code = %q, want root_protected", c)
		}
	})

	t.Run("not enrolled is a conflict", func(t *testing.T) {
		env := newMFAEnv(t)
		id, _, _, _ := env.staff(t, []string{"viewer"}, false)
		token := env.issueAccountToken(t, id, "MFA Staff", []string{"viewer"})

		status, _, body := accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/mfa/disable", token, nil,
			`{"code":"123456"}`)
		if status != http.StatusConflict {
			t.Fatalf("not-enrolled status = %d, want 409 (body %s)", status, body)
		}
		if c := decodeAPIError(t, body).Error.Code; c != "mfa_not_enrolled" {
			t.Errorf("not-enrolled code = %q, want mfa_not_enrolled", c)
		}
	})

	t.Run("wrong code is unauthorized", func(t *testing.T) {
		env := newMFAEnv(t)
		id, _, _, secret := env.staff(t, []string{"viewer"}, true)
		token := env.issueAccountToken(t, id, "MFA Staff", []string{"viewer"})
		wrong := "000000"
		if wrong == env.code(secret) {
			wrong = "000001"
		}

		status, _, body := accountReq(t, http.MethodPost, env.h.public, "/admin-auth/account/mfa/disable", token, nil,
			fmt.Sprintf(`{"code":%q}`, wrong))
		if status != http.StatusUnauthorized {
			t.Fatalf("wrong code status = %d, want 401 (body %s)", status, body)
		}
		if c := decodeAPIError(t, body).Error.Code; c != "invalid_code" {
			t.Errorf("wrong code = %q, want invalid_code", c)
		}
	})
}

func TestAccountEndpointsRequireActiveBearer(t *testing.T) {
	env := newMFAEnv(t)
	_, _, _, _ = env.staff(t, []string{"viewer"}, false)

	endpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/admin-auth/account/password", `{"current_password":"x","new_password":"y"}`},
		{http.MethodGet, "/admin-auth/account/sessions", ""},
		{http.MethodPost, "/admin-auth/account/sessions/revoke-others", ""},
		{http.MethodPost, "/admin-auth/account/mfa/recovery-codes", `{"code":"123456"}`},
		{http.MethodPost, "/admin-auth/account/mfa/disable", `{"code":"123456"}`},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path+"/no-token", func(t *testing.T) {
			status, _, body := accountReq(t, ep.method, env.h.public, ep.path, "", nil, ep.body)
			if status != http.StatusUnauthorized {
				t.Fatalf("no token status = %d, want 401 (body %s)", status, body)
			}
		})
		t.Run(ep.method+" "+ep.path+"/bad-token", func(t *testing.T) {
			status, _, body := accountReq(t, ep.method, env.h.public, ep.path, "not.a.jwt", nil, ep.body)
			if status != http.StatusUnauthorized {
				t.Fatalf("bad token status = %d, want 401 (body %s)", status, body)
			}
		})
	}

	// A disabled account's still-valid token must stop resolving on every route.
	id, _, _, _ := env.staff(t, []string{"viewer"}, false)
	token := env.issueAccountToken(t, id, "MFA Staff", []string{"viewer"})
	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET status = 'disabled' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path+"/disabled", func(t *testing.T) {
			status, _, body := accountReq(t, ep.method, env.h.public, ep.path, token, nil, ep.body)
			if status != http.StatusUnauthorized {
				t.Fatalf("disabled status = %d, want 401 (body %s)", status, body)
			}
		})
	}
}
