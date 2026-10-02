package server_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/admin_auth/internal/mfa"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
)

// token mints an access token for an existing staff row against the env's frozen clock.
func (e *mfaEnv) token(t *testing.T, id, name string, roles []string) string {
	t.Helper()

	tok, err := e.signer.Issue(id, name, roles, e.now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// resetMFA performs the admin reset request for one target and returns status/body/code.
func resetMFA(t *testing.T, env *mfaEnv, target, tok string) (int, string, string) {
	t.Helper()

	status, body := adminCall(t, env.h, http.MethodPost,
		"/api/admin/users/"+target+"/mfa/reset", tok, "")
	return status, body, apiCodeOrEmpty(t, status, body)
}

// TestAdminResetMFARules walks the rule matrix: who may reset whom, and the two
// state-based answers (unknown target, target with nothing enrolled).
func TestAdminResetMFARules(t *testing.T) {
	env := newMFAEnv(t)

	rootID, _ := env.insertRoot(t, "root-pw-reset-rules")
	rootTok := env.token(t, rootID, "Root MFA", []string{"admin"})

	adminID, _, _, _ := env.staff(t, []string{"admin"}, false)
	adminTok := env.token(t, adminID, "MFA Staff", []string{"admin"})

	otherAdminID, _, _, _ := env.staff(t, []string{"admin"}, false)
	liveOpsID, _, _, _ := env.staff(t, []string{"live_ops"}, false)
	viewerID, _, _, _ := env.staff(t, []string{"viewer"}, false)
	viewerTok := env.token(t, viewerID, "MFA Staff", []string{"viewer"})
	liveOpsTok := env.token(t, liveOpsID, "MFA Staff", []string{"live_ops"})

	enrolledAdminID, _, _, _ := env.staff(t, []string{"admin"}, true)
	enrolledLiveID, _, _, _ := env.staff(t, []string{"live_ops"}, true)

	type outcome struct {
		status int
		code   string
	}
	cases := []struct {
		name   string
		tok    string
		target string
		want   outcome
	}{
		{"self", adminTok, adminID, outcome{http.StatusForbidden, "self_modification"}},
		{"root target", adminTok, rootID, outcome{http.StatusForbidden, "root_protected"}},
		{"admin resets admin", adminTok, otherAdminID, outcome{http.StatusForbidden, "insufficient_role"}},
		{"root resets admin", rootTok, enrolledAdminID, outcome{http.StatusOK, ""}},
		{"admin resets live_ops", adminTok, enrolledLiveID, outcome{http.StatusOK, ""}},
		{"viewer caller", viewerTok, liveOpsID, outcome{http.StatusForbidden, "insufficient_role"}},
		{"live_ops caller", liveOpsTok, liveOpsID, outcome{http.StatusForbidden, "insufficient_role"}},
		{"unknown", rootTok, "00000000-0000-0000-0000-000000000000", outcome{http.StatusNotFound, "not_found"}},
		{"nothing enrolled", adminTok, liveOpsID, outcome{http.StatusConflict, "mfa_not_enrolled"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, code := resetMFA(t, env, c.target, c.tok)
			if status != c.want.status {
				t.Fatalf("status = %d, want %d (body %s)", status, c.want.status, body)
			}
			if code != c.want.code {
				t.Errorf("code = %q, want %q", code, c.want.code)
			}
			if status == http.StatusOK {
				got := decodeJSON[testAdminUser](t, body)
				if got.ID != c.target {
					t.Errorf("id = %q, want %s", got.ID, c.target)
				}
				if got.MFAEnrolled {
					t.Error("mfa_enrolled = true after a reset")
				}
			}
		})
	}
}

// TestAdminResetMFAEffect proves one reset removes every credential of the target's
// second factor and audits the work exactly once.
func TestAdminResetMFAEffect(t *testing.T) {
	env := newMFAEnv(t)
	ctx := context.Background()

	rootID, _ := env.insertRoot(t, "root-pw-reset-effect")
	rootTok := env.token(t, rootID, "Root MFA", []string{"admin"})

	id, email, pw, secret := env.staff(t, []string{"live_ops"}, true)

	const codeCount = 3
	codes, err := mfa.RecoveryCodes(codeCount)
	if err != nil {
		t.Fatalf("RecoveryCodes: %v", err)
	}
	for _, code := range codes {
		if _, err := env.db.Pool.Exec(ctx,
			`INSERT INTO mfa_recovery_code (user_id, code_hash) VALUES ($1::uuid, $2)`,
			id, mfa.HashRecoveryCode(code)); err != nil {
			t.Fatalf("insert recovery code: %v", err)
		}
	}

	// An open verify ticket, as a challenged login would leave behind.
	ch := loginTicket(t, env, email, pw)

	// A live refresh session, addressed by a raw cookie the test can replay.
	const rawCookie = "reset-effect-refresh-cookie"
	cookieHash := sha256.Sum256([]byte(rawCookie))
	if _, err := env.db.Pool.Exec(ctx,
		`INSERT INTO refresh_session (family_id, user_id, token_hash, expires_at)
		 VALUES (gen_random_uuid(), $1::uuid, $2, now() + interval '1 hour')`,
		id, cookieHash[:]); err != nil {
		t.Fatalf("insert refresh session: %v", err)
	}

	status, body, code := resetMFA(t, env, id, rootTok)
	if status != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body %s)", status, body)
	}
	if code != "" {
		t.Fatalf("reset code = %q, want empty", code)
	}
	if got := decodeJSON[testAdminUser](t, body); got.MFAEnrolled {
		t.Error("response mfa_enrolled = true after reset")
	}

	// The factor columns are cleared and the recovery codes are gone.
	var (
		confirmed *time.Time
		stored    []byte
		recovery  int
	)
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT totp_confirmed_at, totp_secret_enc FROM staff_user WHERE id = $1::uuid`, id).
		Scan(&confirmed, &stored); err != nil {
		t.Fatalf("read factor columns: %v", err)
	}
	if confirmed != nil || stored != nil {
		t.Errorf("factor columns after reset = (%v, %v), want (nil, nil)", confirmed, stored)
	}
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM mfa_recovery_code WHERE user_id = $1::uuid`, id).Scan(&recovery); err != nil {
		t.Fatalf("count recovery codes: %v", err)
	}
	if recovery != 0 {
		t.Errorf("recovery code rows after reset = %d, want 0", recovery)
	}

	// Login now completes directly, with tokens and a cookie, and no MFA challenge.
	loginStatus, header, loginBody := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw))
	if loginStatus != http.StatusOK {
		t.Fatalf("login after reset status = %d, want 200 (body %s)", loginStatus, loginBody)
	}
	if strings.Contains(loginBody, "mfa_required") || strings.Contains(loginBody, "mfa_enrollment_required") {
		t.Errorf("login after reset still challenges for MFA: %s", loginBody)
	}
	if got := decodeLoginBody(t, loginBody); got.AccessToken == "" {
		t.Error("login after reset returned no access token")
	}
	cookieFromHeader(t, header, testRefreshCookieName)

	// The ticket minted before the reset is burned even with a valid code.
	status, _, verifyBody := postJSON(t, env.h.public, "/admin-auth/mfa/verify", "",
		fmt.Sprintf(`{"mfa_ticket":%q,"code":%q}`, ch.MFATicket, env.code(secret)))
	if status != http.StatusUnauthorized {
		t.Fatalf("old ticket status = %d, want 401 (body %s)", status, verifyBody)
	}

	// The old recovery code is gone: a fresh ticket finds no confirmed factor and
	// refuses it outright, and the row is no longer present.
	freshTicket, err := env.db.CreateMFATicket(ctx, id, store.MFAPurposeVerify, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateMFATicket: %v", err)
	}
	status, _, verifyBody = postJSON(t, env.h.public, "/admin-auth/mfa/verify", "",
		fmt.Sprintf(`{"mfa_ticket":%q,"code":%q}`, freshTicket, codes[0]))
	if status != http.StatusUnauthorized {
		t.Fatalf("old recovery code status = %d, want 401 (body %s)", status, verifyBody)
	}

	// The old refresh cookie is refused.
	refreshStatus, _, refreshBody := postRefresh(t, env.h.public,
		&http.Cookie{Name: testRefreshCookieName, Value: rawCookie})
	if refreshStatus != http.StatusUnauthorized {
		t.Fatalf("old refresh cookie status = %d, want 401 (body %s)", refreshStatus, refreshBody)
	}

	// Exactly one mfa.reset row, written by the root caller, with the counts.
	var (
		audits       int
		actorID      string
		target       string
		hadFactor    bool
		removed      int
		sessionsDown int
	)
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT count(*), coalesce(max(actor_id::text), ''), coalesce(max(target), ''),
		        coalesce(bool_or((details->>'had_factor')::boolean), false),
		        coalesce(max((details->>'recovery_codes_removed')::int), 0),
		        coalesce(max((details->>'sessions_revoked')::int), 0)
		 FROM audit_log WHERE action = 'mfa.reset' AND target = $1`,
		id).Scan(&audits, &actorID, &target, &hadFactor, &removed, &sessionsDown); err != nil {
		t.Fatalf("read mfa.reset audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("mfa.reset rows = %d, want 1", audits)
	}
	if actorID != rootID {
		t.Errorf("mfa.reset actor = %q, want %s", actorID, rootID)
	}
	if target != id {
		t.Errorf("mfa.reset target = %q, want %s", target, id)
	}
	if !hadFactor {
		t.Error("mfa.reset had_factor = false, want true for a confirmed factor")
	}
	if removed != codeCount {
		t.Errorf("recovery_codes_removed = %d, want %d", removed, codeCount)
	}
	if sessionsDown != 1 {
		t.Errorf("sessions_revoked = %d, want 1", sessionsDown)
	}
}

// TestAdminResetMFAAdminMustReenroll is the D5 consequence: after root clears an
// admin's factor, that admin's next login asks it to enroll instead of signing in.
func TestAdminResetMFAAdminMustReenroll(t *testing.T) {
	env := newMFAEnv(t)

	rootID, _ := env.insertRoot(t, "root-pw-reset-admin")
	rootTok := env.token(t, rootID, "Root MFA", []string{"admin"})

	id, email, pw, _ := env.staff(t, []string{"admin"}, true)

	if status, body, code := resetMFA(t, env, id, rootTok); status != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body %s, code %s)", status, body, code)
	}

	status, header, body := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw))
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body %s)", status, body)
	}
	if set := header.Get("Set-Cookie"); set != "" {
		t.Errorf("enrollment-required login set a cookie: %q", set)
	}
	got := decodeMFAChallenge(t, body)
	if !got.MFAEnrollmentRequired || got.MFARequired {
		t.Errorf("challenge = %+v, want mfa_enrollment_required only", got)
	}
	if got.MFATicket == "" {
		t.Error("mfa_enrollment_required returned no ticket")
	}
}

// TestAdminResetMFAConcurrent fires two simultaneous resets at one target. The
// FOR UPDATE lock serialises them: the winner clears the row, the loser re-reads the
// cleared row and answers 409 mfa_not_enrolled. Exactly one audit row is written.
func TestAdminResetMFAConcurrent(t *testing.T) {
	env := newMFAEnv(t)

	rootID, _ := env.insertRoot(t, "root-pw-reset-concurrent")
	rootTok := env.token(t, rootID, "Root MFA", []string{"admin"})
	id, _, _, _ := env.staff(t, []string{"live_ops"}, true)

	const n = 2
	var wg sync.WaitGroup
	statuses := make([]int, n)
	codes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, body := adminCall(t, env.h, http.MethodPost,
				"/api/admin/users/"+id+"/mfa/reset", rootTok, "")
			statuses[i] = status
			codes[i] = apiCodeOrEmpty(t, status, body)
		}(i)
	}
	wg.Wait()

	ok, conflict := 0, 0
	for i := 0; i < n; i++ {
		switch statuses[i] {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
			if codes[i] != "mfa_not_enrolled" {
				t.Errorf("attempt %d conflict code = %q, want mfa_not_enrolled", i, codes[i])
			}
		default:
			t.Fatalf("attempt %d status = %d, want 200 or 409", i, statuses[i])
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Errorf("outcomes = %d ok / %d conflict, want 1 ok / %d conflict", ok, conflict, n-1)
	}

	var audits int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'mfa.reset' AND target = $1`, id).Scan(&audits); err != nil {
		t.Fatalf("count mfa.reset audits: %v", err)
	}
	if audits != 1 {
		t.Errorf("mfa.reset rows after concurrent resets = %d, want 1", audits)
	}
}
