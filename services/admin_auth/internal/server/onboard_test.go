package server_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/admin_auth/internal/mfa"
	"github.com/otomo-live/otomo/services/admin_auth/internal/server"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// onboardEnv is the MFA harness (frozen TOTP clock and sealing key included) plus a
// root actor to create the links under test. Links are always minted through the real
// user-management routes, so the real token hashing is exercised rather than a
// test-side substitute.
type onboardEnv struct {
	*mfaEnv
	rootID  string
	rootTok string
}

func newOnboardEnv(t *testing.T) *onboardEnv {
	t.Helper()

	db := loginDB(t)
	cfg := adminTestConfig()
	signer, pub, raw := newLoginSigner(t, cfg)
	verifier := token.NewVerifier(cfg.Issuer, cfg.Audience, []token.PublicKey{{Kid: loginKid, Key: pub}})
	key, err := mfa.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)

	h := newHarnessWithConfig(t, cfg, server.Deps{
		JWKS:     fakeJWKS{raw: raw},
		Signer:   signer,
		Verifier: verifier,
		Store:    db,
		MFAKey:   key,
		MFANow:   func() time.Time { return now },
	})
	e := &onboardEnv{mfaEnv: &mfaEnv{
		sessionEnv: &sessionEnv{h: h, db: db, cfg: cfg, signer: signer, pub: pub},
		key:        key,
		now:        now,
	}}
	e.rootID, _ = e.insertRoot(t, "root-onboard-password-1234")
	e.rootTok, err = e.signer.Issue(e.rootID, "Root Onboard", []string{"admin"}, now)
	if err != nil {
		t.Fatalf("Issue root token: %v", err)
	}
	// Accounts created by redeeming an invite carry created_by = root, so they must
	// go before insertRoot's cleanup deletes the root row. This cleanup was
	// registered after insertRoot's, so LIFO runs it first.
	t.Cleanup(func() {
		_, _ = e.db.Pool.Exec(context.Background(),
			`DELETE FROM staff_user WHERE created_by = $1::uuid`, e.rootID)
	})
	return e
}

// createInvite mints an invite link through POST /api/admin/users and returns its id
// and fragment token.
func (e *onboardEnv) createInvite(t *testing.T, email, name, role string) (inviteID, token string) {
	t.Helper()
	status, body := adminCall(t, e.h, http.MethodPost, "/api/admin/users", e.rootTok,
		fmt.Sprintf(`{"email":%q,"name":%q,"role":%q}`, email, name, role))
	if status != http.StatusCreated {
		t.Fatalf("create invite: status = %d, want 201 (body %s)", status, body)
	}
	created := decodeJSON[testInviteCreated](t, body)
	return created.InviteID, fragmentToken(t, created.InviteURL)
}

// createReset mints a reset link through POST /api/admin/users/{id}/reset.
func (e *onboardEnv) createReset(t *testing.T, targetID string) (inviteID, token string) {
	t.Helper()
	status, body := adminCall(t, e.h, http.MethodPost, "/api/admin/users/"+targetID+"/reset", e.rootTok, "")
	if status != http.StatusCreated {
		t.Fatalf("create reset: status = %d, want 201 (body %s)", status, body)
	}
	created := decodeJSON[testResetCreated](t, body)
	return created.InviteID, fragmentToken(t, created.ResetURL)
}

// postOnboard performs one JSON request against a public onboarding route.
func postOnboard(t *testing.T, h *harness, path, body string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.public+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

// redeem posts an invite or reset redemption.
func redeem(t *testing.T, h *harness, token, pw string) (int, http.Header, string) {
	t.Helper()
	return postOnboard(t, h, "/admin-auth/onboard", fmt.Sprintf(`{"token":%q,"password":%q}`, token, pw))
}

type testOnboardLookup struct {
	Purpose   string    `json:"purpose"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      *string   `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
}

func lookupLink(t *testing.T, h *harness, token string) (int, testOnboardLookup, apiErrorBody) {
	t.Helper()
	status, _, body := postOnboard(t, h, "/admin-auth/onboard/lookup", fmt.Sprintf(`{"token":%q}`, token))
	var got testOnboardLookup
	if status == http.StatusOK {
		got = decodeJSON[testOnboardLookup](t, body)
	}
	return status, got, decodeAPIError(t, body)
}

func TestOnboardLookup(t *testing.T) {
	env := newOnboardEnv(t)
	ctx := context.Background()

	inviteEmail := uniqueLoginEmail("lookup-invite")
	_, inviteTok := env.createInvite(t, inviteEmail, "Lookup Invitee", "live_ops")

	status, got, _ := lookupLink(t, env.h, inviteTok)
	if status != http.StatusOK {
		t.Fatalf("invite lookup status = %d, want 200", status)
	}
	if got.Purpose != "invite" || got.Email != inviteEmail || got.Name != "Lookup Invitee" {
		t.Errorf("invite lookup = %+v, want invite/%s/Lookup Invitee", got, inviteEmail)
	}
	if got.Role == nil || *got.Role != "live_ops" {
		t.Errorf("invite role = %v, want live_ops", got.Role)
	}
	if got.ExpiresAt.IsZero() || !got.ExpiresAt.After(time.Now()) {
		t.Errorf("invite expires_at = %s, want a future time", got.ExpiresAt)
	}

	resetEmail := uniqueLoginEmail("lookup-reset")
	resetID := insertStaff(t, env.db, resetEmail, "Lookup Reset", "old-password-1234", []string{"viewer"})
	_, resetTok := env.createReset(t, resetID)

	status, got, _ = lookupLink(t, env.h, resetTok)
	if status != http.StatusOK {
		t.Fatalf("reset lookup status = %d, want 200", status)
	}
	if got.Purpose != "reset" || got.Email != resetEmail || got.Name != "Lookup Reset" {
		t.Errorf("reset lookup = %+v, want reset/%s/Lookup Reset", got, resetEmail)
	}
	if got.Role != nil {
		t.Errorf("reset role = %v, want null", got.Role)
	}

	// Every unusable link gets the same 404 code and message. request_id necessarily
	// differs per request, so the comparison is on the stable pair.
	invalid := func(name, token string) {
		t.Helper()
		status, _, errBody := lookupLink(t, env.h, token)
		if status != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", name, status)
		}
		if errBody.Error.Code != "invalid_link" || errBody.Error.Message == "" {
			t.Errorf("%s: error = %+v, want invalid_link", name, errBody.Error)
		}
	}

	// used: redeem a fresh invite, then look it up.
	usedEmail := uniqueLoginEmail("lookup-used")
	_, usedTok := env.createInvite(t, usedEmail, "Used", "viewer")
	if status, _, body := redeem(t, env.h, usedTok, "used-password-1234"); status != http.StatusOK {
		t.Fatalf("redeem for used case: status = %d (body %s)", status, body)
	}
	invalid("used", usedTok)

	// revoked.
	revokedID, revokedTok := env.createInvite(t, uniqueLoginEmail("lookup-revoked"), "Revoked", "viewer")
	if status, body := adminCall(t, env.h, http.MethodDelete, "/api/admin/users/invites/"+revokedID, env.rootTok, ""); status != http.StatusNoContent {
		t.Fatalf("revoke: status = %d (body %s)", status, body)
	}
	invalid("revoked", revokedTok)

	// expired: move both timestamps into the past so the expires_at > created_at
	// check still holds.
	expiredID, expiredTok := env.createInvite(t, uniqueLoginEmail("lookup-expired"), "Expired", "viewer")
	if _, err := env.db.Pool.Exec(ctx,
		`UPDATE staff_invite SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'
		 WHERE id = $1::uuid`, expiredID); err != nil {
		t.Fatalf("expire invite: %v", err)
	}
	invalid("expired", expiredTok)

	// unknown.
	invalid("unknown", "not-a-real-token")

	// A used link and an unknown one must not be distinguishable.
	_, _, usedErr := lookupLink(t, env.h, usedTok)
	_, _, unknownErr := lookupLink(t, env.h, "another-unknown-token")
	if usedErr.Error.Code != unknownErr.Error.Code || usedErr.Error.Message != unknownErr.Error.Message {
		t.Errorf("used error = %+v, unknown = %+v; want identical", usedErr.Error, unknownErr.Error)
	}
}

func TestOnboardInviteRedeem(t *testing.T) {
	env := newOnboardEnv(t)
	ctx := context.Background()

	email := uniqueLoginEmail("redeem-invite")
	inviteID, tok := env.createInvite(t, email, "Redeemed Invitee", "live_ops")

	const pw = "correct-horse-battery-staple"
	status, header, body := redeem(t, env.h, tok, pw)
	if status != http.StatusOK {
		t.Fatalf("redeem status = %d, want 200 (body %s)", status, body)
	}
	got := decodeLoginBody(t, body)
	if got.AccessToken == "" {
		t.Error("access_token is empty")
	}
	if len(got.User.Roles) != 1 || got.User.Roles[0] != "live_ops" {
		t.Errorf("roles = %v, want [live_ops]", got.User.Roles)
	}

	resp := &http.Response{Header: header}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == testRefreshCookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("no %s cookie on a successful redemption", testRefreshCookieName)
	}

	var userID, createdBy string
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT id::text, created_by::text FROM staff_user WHERE lower(email) = lower($1)`,
		email).Scan(&userID, &createdBy); err != nil {
		t.Fatalf("read onboarded user: %v", err)
	}
	if createdBy != env.rootID {
		t.Errorf("created_by = %q, want the invite creator %q", createdBy, env.rootID)
	}
	t.Cleanup(func() {
		_, _ = env.db.Pool.Exec(context.Background(), `DELETE FROM staff_user WHERE id = $1::uuid`, userID)
	})

	var used bool
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT used_at IS NOT NULL FROM staff_invite WHERE id = $1::uuid`, inviteID).Scan(&used); err != nil {
		t.Fatalf("read invite state: %v", err)
	}
	if !used {
		t.Error("the invite was not marked used")
	}

	// A second redemption of the same link is refused.
	if status, _, _ := redeem(t, env.h, tok, pw); status != http.StatusNotFound {
		t.Errorf("second redeem status = %d, want 404", status)
	}

	// The new account can log in with the password it just set.
	if status, _, body := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw)); status != http.StatusOK {
		t.Fatalf("login after onboarding status = %d, want 200 (body %s)", status, body)
	}
}

func TestOnboardResetRedeem(t *testing.T) {
	env := newOnboardEnv(t)
	ctx := context.Background()

	email := uniqueLoginEmail("redeem-reset")
	const (
		oldPw = "old-password-1234"
		newPw = "new-password-5678"
	)
	id := insertStaff(t, env.db, email, "Reset Target", oldPw, []string{"viewer"})

	oldSession := sha256.Sum256([]byte("old-session-token-" + email))
	if _, err := env.db.Pool.Exec(ctx,
		`INSERT INTO refresh_session (family_id, user_id, token_hash, expires_at)
		 VALUES (gen_random_uuid(), $1::uuid, $2, now() + interval '1 hour')`,
		id, oldSession[:]); err != nil {
		t.Fatalf("insert old session: %v", err)
	}
	if _, err := env.db.Pool.Exec(ctx,
		`UPDATE staff_user SET failed_logins = 4, locked_until = now() + interval '10 minutes'
		 WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("seed lockout: %v", err)
	}

	_, tok := env.createReset(t, id)
	status, header, body := redeem(t, env.h, tok, newPw)
	if status != http.StatusOK {
		t.Fatalf("reset redeem status = %d, want 200 (body %s)", status, body)
	}
	resp := &http.Response{Header: header}
	foundCookie := false
	for _, c := range resp.Cookies() {
		if c.Name == testRefreshCookieName && c.Value != "" {
			foundCookie = true
		}
	}
	if !foundCookie {
		t.Errorf("no %s cookie on a successful reset", testRefreshCookieName)
	}

	// Lockout is cleared by the redemption itself, before any later login.
	var (
		failedLogins int
		lockedUntil  *time.Time
	)
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT failed_logins, locked_until FROM staff_user WHERE id = $1::uuid`, id).
		Scan(&failedLogins, &lockedUntil); err != nil {
		t.Fatalf("read account after reset: %v", err)
	}
	if failedLogins != 0 || lockedUntil != nil {
		t.Errorf("after reset: failed_logins = %d, locked_until = %v; want 0 and NULL", failedLogins, lockedUntil)
	}

	// The old session was revoked.
	var revoked bool
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT revoked_at IS NOT NULL FROM refresh_session WHERE token_hash = $1`, oldSession[:]).Scan(&revoked); err != nil {
		t.Fatalf("read old session: %v", err)
	}
	if !revoked {
		t.Error("the old refresh session was not revoked")
	}

	// The old password no longer works; the new one does.
	if status, _, _ := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, oldPw)); status != http.StatusUnauthorized {
		t.Errorf("old password status = %d, want 401", status)
	}
	if status, _, body := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, newPw)); status != http.StatusOK {
		t.Fatalf("new password status = %d, want 200 (body %s)", status, body)
	}
}

func TestOnboardInviteAdminRequiresEnrollment(t *testing.T) {
	env := newOnboardEnv(t)

	email := uniqueLoginEmail("onboard-admin")
	_, tok := env.createInvite(t, email, "Onboard Admin", "admin")
	const pw = "onboard-admin-password-1234"

	status, header, body := redeem(t, env.h, tok, pw)
	if status != http.StatusOK {
		t.Fatalf("redeem status = %d, want 200 (body %s)", status, body)
	}
	if set := header.Get("Set-Cookie"); set != "" {
		t.Errorf("admin invite redeem set a cookie: %q", set)
	}
	ch := decodeMFAChallenge(t, body)
	if !ch.MFAEnrollmentRequired || ch.MFARequired {
		t.Errorf("challenge = %+v, want mfa_enrollment_required only", ch)
	}
	if ch.AccessToken != "" || strings.Contains(body, "access_token") {
		t.Error("the enrollment challenge carries an access token")
	}
	if purpose := ticketPurpose(t, env.mfaEnv, ch.MFATicket); purpose != "enroll" {
		t.Errorf("ticket purpose = %q, want enroll", purpose)
	}

	// The new password is live immediately: a login demands enrollment too, before
	// any factor is confirmed. (This also proves login still enforces D5.)
	if login := loginTicket(t, env.mfaEnv, email, pw); !login.MFAEnrollmentRequired {
		t.Errorf("login after admin onboarding = %+v, want mfa_enrollment_required", login)
	}

	// enroll + confirm with the onboarding ticket ends signed in.
	status, _, body = postJSON(t, env.h.public, "/admin-auth/mfa/enroll", "",
		fmt.Sprintf(`{"mfa_ticket":%q}`, ch.MFATicket))
	if status != http.StatusOK {
		t.Fatalf("enroll status = %d, want 200 (body %s)", status, body)
	}
	enrolled := decodeJSON[mfaEnrollWire](t, body)
	secret, err := mfa.DecodeSecret(enrolled.Secret)
	if err != nil {
		t.Fatalf("DecodeSecret: %v", err)
	}

	status, header, body = postJSON(t, env.h.public, "/admin-auth/mfa/confirm", "",
		fmt.Sprintf(`{"mfa_ticket":%q,"code":%q}`, ch.MFATicket, env.code(secret)))
	if status != http.StatusOK {
		t.Fatalf("confirm status = %d, want 200 (body %s)", status, body)
	}
	confirmed := decodeJSON[mfaConfirmWire](t, body)
	if confirmed.AccessToken == "" {
		t.Error("confirm with the onboarding ticket returned no access token")
	}
	cookieFromHeader(t, header, testRefreshCookieName)
}

func TestOnboardResetWithConfirmedTOTPRequiresVerify(t *testing.T) {
	env := newOnboardEnv(t)
	id, email, oldPw, secret := env.staff(t, []string{"viewer"}, true)

	_, tok := env.createReset(t, id)
	const newPw = "reset-mfa-password-5678"

	status, header, body := redeem(t, env.h, tok, newPw)
	if status != http.StatusOK {
		t.Fatalf("redeem status = %d, want 200 (body %s)", status, body)
	}
	if set := header.Get("Set-Cookie"); set != "" {
		t.Errorf("reset redeem with a confirmed factor set a cookie: %q", set)
	}
	ch := decodeMFAChallenge(t, body)
	if !ch.MFARequired || ch.MFAEnrollmentRequired {
		t.Errorf("challenge = %+v, want mfa_required only", ch)
	}
	if ch.AccessToken != "" || strings.Contains(body, "access_token") {
		t.Error("the verify challenge carries an access token")
	}
	if purpose := ticketPurpose(t, env.mfaEnv, ch.MFATicket); purpose != "verify" {
		t.Errorf("ticket purpose = %q, want verify", purpose)
	}

	// The reset changed the password but must not have bypassed the factor: the
	// new password still demands a code, and the old one is dead.
	if login := loginTicket(t, env.mfaEnv, email, newPw); !login.MFARequired {
		t.Errorf("login after reset = %+v, want mfa_required", login)
	}
	if status, _, _ := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, oldPw)); status != http.StatusUnauthorized {
		t.Errorf("old password status = %d, want 401", status)
	}

	// A fresh code on the onboarding ticket finishes the sign-in.
	status, header, body = postJSON(t, env.h.public, "/admin-auth/mfa/verify", "",
		fmt.Sprintf(`{"mfa_ticket":%q,"code":%q}`, ch.MFATicket, env.code(secret)))
	if status != http.StatusOK {
		t.Fatalf("verify status = %d, want 200 (body %s)", status, body)
	}
	if got := decodeLoginBody(t, body); got.AccessToken == "" {
		t.Error("verify returned no access token")
	}
	cookieFromHeader(t, header, testRefreshCookieName)
}

func TestOnboardPasswordRules(t *testing.T) {
	env := newOnboardEnv(t)
	ctx := context.Background()

	email := uniqueLoginEmail("longlocalpart")
	local := email[:strings.IndexByte(email, '@')]
	inviteID, tok := env.createInvite(t, email, "Rules", "viewer")

	cases := []struct {
		name string
		pw   string
		want string
	}{
		{"11 runes", strings.Repeat("a", 11), "at least 12"},
		{"129 runes", strings.Repeat("a", 129), "at most 128"},
		{"equals local part", local, "email address"},
		{"common password", "password1234", "too common"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, _, body := redeem(t, env.h, tok, c.pw)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", status, body)
			}
			got := decodeAPIError(t, body)
			if got.Error.Code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", got.Error.Code)
			}
			if !strings.Contains(got.Error.Message, c.want) {
				t.Errorf("message = %q, want it to name %q", got.Error.Message, c.want)
			}
		})
	}

	// A rejected password never consumes the link.
	var used bool
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT used_at IS NOT NULL FROM staff_invite WHERE id = $1::uuid`, inviteID).Scan(&used); err != nil {
		t.Fatalf("read invite state: %v", err)
	}
	if used {
		t.Error("the link was consumed by a password that was refused")
	}
}

func TestOnboardConcurrentRedeem(t *testing.T) {
	env := newOnboardEnv(t)
	ctx := context.Background()

	email := uniqueLoginEmail("redeem-race")
	const pw = "race-password-1234"
	_, tok := env.createInvite(t, email, "Race", "viewer")

	const workers = 3
	type result struct {
		status int
		err    error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, env.h.public+"/admin-auth/onboard",
				strings.NewReader(fmt.Sprintf(`{"token":%q,"password":%q}`, tok, pw)))
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			results <- result{status: resp.StatusCode}
		}()
	}
	wg.Wait()
	close(results)

	ok, notFound := 0, 0
	for res := range results {
		if res.err != nil {
			t.Fatalf("concurrent redeem: %v", res.err)
		}
		switch res.status {
		case http.StatusOK:
			ok++
		case http.StatusNotFound:
			notFound++
		default:
			t.Errorf("concurrent redeem status = %d, want 200 or 404", res.status)
		}
	}
	if ok != 1 || notFound != workers-1 {
		t.Errorf("statuses = %d ok / %d 404, want 1 ok / %d 404", ok, notFound, workers-1)
	}

	var users int
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM staff_user WHERE lower(email) = lower($1)`, email).Scan(&users); err != nil {
		t.Fatalf("count onboarded users: %v", err)
	}
	if users != 1 {
		t.Errorf("staff_user rows = %d, want exactly 1", users)
	}
	t.Cleanup(func() {
		_, _ = env.db.Pool.Exec(context.Background(), `DELETE FROM staff_user WHERE lower(email) = lower($1)`, email)
	})
}

func TestOnboardInviteEmailConflict(t *testing.T) {
	env := newOnboardEnv(t)
	ctx := context.Background()

	email := uniqueLoginEmail("conflict")
	inviteID, tok := env.createInvite(t, email, "Conflict", "viewer")
	// The address becomes a user after the invite was created.
	insertStaff(t, env.db, email, "Conflict Existing", "existing-password-1234", []string{"viewer"})

	status, _, body := redeem(t, env.h, tok, "brand-new-password-1234")
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", status, body)
	}
	if code := decodeAPIError(t, body).Error.Code; code != "already_exists" {
		t.Errorf("code = %q, want already_exists", code)
	}

	var used bool
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT used_at IS NOT NULL FROM staff_invite WHERE id = $1::uuid`, inviteID).Scan(&used); err != nil {
		t.Fatalf("read invite state: %v", err)
	}
	if !used {
		t.Error("the conflicted link was not marked used")
	}
}

func TestOnboardBadBodies(t *testing.T) {
	env := newOnboardEnv(t)

	cases := []struct {
		name string
		path string
		body string
		code string
	}{
		{"lookup missing token", "/admin-auth/onboard/lookup", `{}`, "validation_failed"},
		{"lookup unknown field", "/admin-auth/onboard/lookup", `{"token":"x","extra":true}`, "invalid_body"},
		{"lookup empty body", "/admin-auth/onboard/lookup", "", "invalid_body"},
		{"onboard missing token", "/admin-auth/onboard", `{"password":"a-long-enough-password"}`, "validation_failed"},
		{"onboard missing password", "/admin-auth/onboard", `{"token":"x"}`, "validation_failed"},
		{"onboard unknown field", "/admin-auth/onboard", `{"token":"x","password":"a-long-enough-password","extra":1}`, "invalid_body"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, _, body := postOnboard(t, env.h, c.path, c.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", status, body)
			}
			if code := decodeAPIError(t, body).Error.Code; code != c.code {
				t.Errorf("code = %q, want %q", code, c.code)
			}
		})
	}
}
