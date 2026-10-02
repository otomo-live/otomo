package server_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/otomo-live/otomo/services/admin_auth/internal/config"
	"github.com/otomo-live/otomo/services/admin_auth/internal/server"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// adminTestConfig is loginConfig plus the user-management settings the routes read.
func adminTestConfig() config.Config {
	cfg := loginConfig()
	cfg.InviteTTL = 72 * time.Hour
	cfg.PublicURL = "https://admin.example.test"
	return cfg
}

// adminEnv is the login harness plus the database and signer the user-management
// tests drive. Tokens are minted directly for a real staff row, because the middleware
// re-reads the row and the token's own roles are irrelevant.
type adminEnv struct {
	h      *harness
	db     *store.DB
	cfg    config.Config
	signer *token.Signer
	pub    ed25519.PublicKey
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()

	db := loginDB(t)
	cfg := adminTestConfig()
	signer, pub, raw := newLoginSigner(t, cfg)
	verifier := token.NewVerifier(cfg.Issuer, cfg.Audience, []token.PublicKey{{Kid: loginKid, Key: pub}})
	h := newHarnessWithConfig(t, cfg, server.Deps{
		JWKS:     fakeJWKS{raw: raw},
		Signer:   signer,
		Verifier: verifier,
		Store:    db,
	})
	return &adminEnv{h: h, db: db, cfg: cfg, signer: signer, pub: pub}
}

// issue mints an access token for an existing staff row.
func (e *adminEnv) issue(t *testing.T, id, name string, roles []string) string {
	t.Helper()
	tok, err := e.signer.Issue(id, name, roles, time.Now())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// staff inserts an active account and returns its id, email and a token.
func (e *adminEnv) staff(t *testing.T, roles ...string) (id, email, tok string) {
	t.Helper()
	email = uniqueLoginEmail("adminstaff")
	name := "Staff " + email
	id = insertStaff(t, e.db, email, name, "password-"+email, roles)
	return id, email, e.issue(t, id, name, roles)
}

// newRoot clears any pre-existing root row and inserts a fresh one, so a test can
// assume exactly the root it created exists. Cleanup removes the row and any invite
// that references it.
func (e *adminEnv) newRoot(t *testing.T) (id, email, tok string) {
	t.Helper()
	ctx := context.Background()

	_, _ = e.db.Pool.Exec(ctx,
		`DELETE FROM staff_invite WHERE created_by IN (SELECT id FROM staff_user WHERE is_root)
		    OR user_id IN (SELECT id FROM staff_user WHERE is_root)`)
	_, _ = e.db.Pool.Exec(ctx, `DELETE FROM staff_user WHERE is_root`)

	email = uniqueLoginEmail("adminroot")
	name := "Root " + email
	if err := e.db.Pool.QueryRow(ctx,
		`INSERT INTO staff_user (email, name, password_hash, roles, is_root)
		 VALUES ($1, $2, 'x', ARRAY['admin'], true) RETURNING id::text`,
		email, name).Scan(&id); err != nil {
		t.Fatalf("insert root: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.db.Pool.Exec(ctx, `DELETE FROM staff_invite WHERE created_by = $1::uuid OR user_id = $1::uuid`, id)
		_, _ = e.db.Pool.Exec(ctx, `DELETE FROM staff_user WHERE id = $1::uuid`, id)
	})
	return id, email, e.issue(t, id, name, []string{"admin"})
}

// adminCall performs one JSON request with an optional bearer token.
func adminCall(t *testing.T, h *harness, method, path, tok, body string) (int, string) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.public+path, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, string(raw)
}

func fragmentToken(t *testing.T, link string) string {
	t.Helper()
	const marker = "#token="
	i := strings.Index(link, marker)
	if i < 0 {
		t.Fatalf("link %q has no %s fragment", link, marker)
	}
	return link[i+len(marker):]
}

type testAdminUser struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Roles       []string   `json:"roles"`
	IsRoot      bool       `json:"is_root"`
	Status      string     `json:"status"`
	MFAEnrolled bool       `json:"mfa_enrolled"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

type testAdminUsers struct {
	Users []testAdminUser `json:"users"`
}

type testInvite struct {
	ID        string    `json:"id"`
	Purpose   string    `json:"purpose"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      *string   `json:"role"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type testInvites struct {
	Invites []testInvite `json:"invites"`
}

type testInviteCreated struct {
	InviteID  string    `json:"invite_id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	InviteURL string    `json:"invite_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type testResetCreated struct {
	InviteID  string    `json:"invite_id"`
	ResetURL  string    `json:"reset_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func decodeJSON[T any](t *testing.T, body string) T {
	t.Helper()
	var got T
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body %q is not JSON: %v", body, err)
	}
	return got
}

func TestAdminUsersAuth(t *testing.T) {
	env := newAdminEnv(t)

	_, _, viewer := env.staff(t, "viewer")
	_, _, liveOps := env.staff(t, "live_ops")
	disabledID, _, disabledTok := env.staff(t, "admin")
	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET status = 'disabled' WHERE id = $1::uuid`, disabledID); err != nil {
		t.Fatalf("disable admin: %v", err)
	}

	cases := []struct {
		name   string
		tok    string
		status int
		code   string
	}{
		{"no token", "", http.StatusUnauthorized, "missing_token"},
		{"viewer", viewer, http.StatusForbidden, "insufficient_role"},
		{"live_ops", liveOps, http.StatusForbidden, "insufficient_role"},
		{"disabled admin", disabledTok, http.StatusUnauthorized, "invalid_token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := adminCall(t, env.h, http.MethodGet, "/api/admin/users", c.tok, "")
			if status != c.status {
				t.Fatalf("status = %d, want %d (body %s)", status, c.status, body)
			}
			if got := decodeAPIError(t, body).Error.Code; got != c.code {
				t.Errorf("code = %q, want %q", got, c.code)
			}
		})
	}
}

func TestAdminListUsersExcludesSecrets(t *testing.T) {
	env := newAdminEnv(t)
	_, _, rootTok := env.newRoot(t)

	id, email, _ := env.staff(t, "viewer")
	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET totp_secret_enc = '\xdeadbeef'::bytea, totp_confirmed_at = now()
		 WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("enrol TOTP: %v", err)
	}

	var hash string
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT password_hash FROM staff_user WHERE id = $1::uuid`, id).Scan(&hash); err != nil {
		t.Fatalf("read password hash: %v", err)
	}

	status, body := adminCall(t, env.h, http.MethodGet, "/api/admin/users", rootTok, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	for _, secret := range []string{"password_hash", "totp_secret", hash} {
		if strings.Contains(body, secret) {
			t.Errorf("list body leaks %q", secret)
		}
	}

	got := decodeJSON[testAdminUsers](t, body)
	var found *testAdminUser
	for i := range got.Users {
		if got.Users[i].ID == id {
			found = &got.Users[i]
		}
	}
	if found == nil {
		t.Fatalf("user %s missing from list", id)
	}
	if found.Email != email {
		t.Errorf("email = %q, want %q", found.Email, email)
	}
	if !found.MFAEnrolled {
		t.Error("mfa_enrolled = false, want true for a confirmed TOTP")
	}
	if found.IsRoot {
		t.Error("is_root = true for a non-root user")
	}
}

func TestAdminInviteCreatesAndHashesTheToken(t *testing.T) {
	env := newAdminEnv(t)
	_, _, rootTok := env.newRoot(t)

	email := uniqueLoginEmail("invitee")
	status, body := adminCall(t, env.h, http.MethodPost, "/api/admin/users", rootTok,
		fmt.Sprintf(`{"email":%q,"name":"New Admin","role":"viewer"}`, email))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", status, body)
	}
	got := decodeJSON[testInviteCreated](t, body)
	if got.Email != email || got.Role != "viewer" {
		t.Errorf("response = %+v, want email %s role viewer", got, email)
	}
	if got.InviteID == "" {
		t.Fatal("invite_id is empty")
	}
	if !strings.HasPrefix(got.InviteURL, env.cfg.PublicURL+"/admin/onboard#token=") {
		t.Errorf("invite_url = %q, want the public URL with a token fragment", got.InviteURL)
	}
	if got.ExpiresAt.Before(time.Now().Add(71 * time.Hour)) {
		t.Errorf("expires_at = %s, want about 72h out", got.ExpiresAt)
	}

	raw := fragmentToken(t, got.InviteURL)
	if raw == "" {
		t.Fatal("invite_url carries no token")
	}
	want := sha256.Sum256([]byte(raw))

	var (
		storedHash []byte
		purpose    string
		role       *string
		name       string
	)
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT token_hash, purpose, role, name FROM staff_invite WHERE id = $1::uuid`, got.InviteID).
		Scan(&storedHash, &purpose, &role, &name); err != nil {
		t.Fatalf("read invite: %v", err)
	}
	if !bytes.Equal(storedHash, want[:]) {
		t.Errorf("token_hash = %x, want sha256 of the fragment token", storedHash)
	}
	if bytes.Equal(storedHash, []byte(raw)) {
		t.Error("the raw invite token was stored")
	}
	if purpose != "invite" || role == nil || *role != "viewer" || name != "New Admin" {
		t.Errorf("row = (%s, %v, %q), want invite/viewer/New Admin", purpose, role, name)
	}

	var audits int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'user.invite' AND target = $1`, got.InviteID).Scan(&audits); err != nil {
		t.Fatalf("count invite audits: %v", err)
	}
	if audits != 1 {
		t.Errorf("user.invite audit rows = %d, want 1", audits)
	}

	// The list route must not replay the token back.
	status, listBody := adminCall(t, env.h, http.MethodGet, "/api/admin/users/invites", rootTok, "")
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", status, listBody)
	}
	if strings.Contains(listBody, raw) {
		t.Error("the invites list leaks the raw token")
	}
	list := decodeJSON[testInvites](t, listBody)
	found := false
	for _, inv := range list.Invites {
		if inv.ID == got.InviteID {
			found = true
			if inv.Role == nil || *inv.Role != "viewer" || inv.Purpose != "invite" {
				t.Errorf("listed invite = %+v, want invite/viewer", inv)
			}
		}
	}
	if !found {
		t.Errorf("invite %s missing from GET /api/admin/users/invites", got.InviteID)
	}
}

func TestAdminInviteValidationAndConflicts(t *testing.T) {
	env := newAdminEnv(t)
	_, _, rootTok := env.newRoot(t)
	_, _, adminTok := env.staff(t, "admin")

	_, existingEmail, _ := env.staff(t, "viewer")
	// Shape failures are 400 for either caller.
	for _, c := range []struct {
		name string
		body string
	}{
		{"bad email", fmt.Sprintf(`{"email":%q,"name":"n","role":"viewer"}`, "no-at-sign")},
		{"name empty", fmt.Sprintf(`{"email":%q,"name":"","role":"viewer"}`, uniqueLoginEmail("v"))},
		{"name too long", fmt.Sprintf(`{"email":%q,"name":%q,"role":"viewer"}`, uniqueLoginEmail("v"), strings.Repeat("a", 101))},
		{"bad role", fmt.Sprintf(`{"email":%q,"name":"n","role":"root"}`, uniqueLoginEmail("v"))},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := adminCall(t, env.h, http.MethodPost, "/api/admin/users", rootTok, c.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", status, body)
			}
			if code := decodeAPIError(t, body).Error.Code; code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
		})
	}

	// A non-root admin cannot grant admin.
	target := uniqueLoginEmail("grantee")
	status, body := adminCall(t, env.h, http.MethodPost, "/api/admin/users", adminTok,
		fmt.Sprintf(`{"email":%q,"name":"Grantee","role":"admin"}`, target))
	if status != http.StatusForbidden {
		t.Fatalf("non-root grant admin status = %d, want 403 (body %s)", status, body)
	}
	gotErr := decodeAPIError(t, body)
	if gotErr.Error.Code != "insufficient_role" || gotErr.Error.Message != "only root can grant admin" {
		t.Errorf("error = %q/%q, want insufficient_role/only root can grant admin", gotErr.Error.Code, gotErr.Error.Message)
	}

	// Root may grant admin.
	status, body = adminCall(t, env.h, http.MethodPost, "/api/admin/users", rootTok,
		fmt.Sprintf(`{"email":%q,"name":"Grantee","role":"admin"}`, target))
	if status != http.StatusCreated {
		t.Fatalf("root grant admin status = %d, want 201 (body %s)", status, body)
	}

	// An address that is already a user is a conflict.
	status, body = adminCall(t, env.h, http.MethodPost, "/api/admin/users", rootTok,
		fmt.Sprintf(`{"email":%q,"name":"Dup","role":"viewer"}`, existingEmail))
	if status != http.StatusConflict || decodeAPIError(t, body).Error.Code != "already_exists" {
		t.Fatalf("duplicate user = %d/%s, want 409 already_exists", status, body)
	}

	// A second invite for the same pending address is a conflict.
	pending := uniqueLoginEmail("pending")
	if status, body = adminCall(t, env.h, http.MethodPost, "/api/admin/users", rootTok,
		fmt.Sprintf(`{"email":%q,"name":"First","role":"viewer"}`, pending)); status != http.StatusCreated {
		t.Fatalf("first invite status = %d, want 201 (body %s)", status, body)
	}
	status, body = adminCall(t, env.h, http.MethodPost, "/api/admin/users", rootTok,
		fmt.Sprintf(`{"email":%q,"name":"Second","role":"viewer"}`, pending))
	if status != http.StatusConflict || decodeAPIError(t, body).Error.Code != "invite_pending" {
		t.Fatalf("pending invite = %d/%s, want 409 invite_pending", status, body)
	}
}

func TestAdminInviteRevoke(t *testing.T) {
	env := newAdminEnv(t)
	_, _, rootTok := env.newRoot(t)

	email := uniqueLoginEmail("revokee")
	_, body := adminCall(t, env.h, http.MethodPost, "/api/admin/users", rootTok,
		fmt.Sprintf(`{"email":%q,"name":"Revokee","role":"viewer"}`, email))
	created := decodeJSON[testInviteCreated](t, body)

	status, revokeBody := adminCall(t, env.h, http.MethodDelete, "/api/admin/users/invites/"+created.InviteID, rootTok, "")
	if status != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204 (body %s)", status, revokeBody)
	}
	status, revokeBody = adminCall(t, env.h, http.MethodDelete, "/api/admin/users/invites/"+created.InviteID, rootTok, "")
	if status != http.StatusNotFound {
		t.Fatalf("second revoke status = %d, want 404 (body %s)", status, revokeBody)
	}

	_, listBody := adminCall(t, env.h, http.MethodGet, "/api/admin/users/invites", rootTok, "")
	for _, inv := range decodeJSON[testInvites](t, listBody).Invites {
		if inv.ID == created.InviteID {
			t.Errorf("revoked invite %s is still listed", created.InviteID)
		}
	}

	var audits int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'invite.revoke' AND target = $1`, created.InviteID).Scan(&audits); err != nil {
		t.Fatalf("count revoke audits: %v", err)
	}
	if audits != 1 {
		t.Errorf("invite.revoke audit rows = %d, want 1", audits)
	}
}

func TestAdminPatchMatrix(t *testing.T) {
	env := newAdminEnv(t)
	rootID, _, rootTok := env.newRoot(t)
	adminID, _, adminTok := env.staff(t, "admin")
	liveOpsID, _, _ := env.staff(t, "live_ops")
	otherAdminID, _, _ := env.staff(t, "admin")

	countUpdates := func(target string) int {
		var n int
		if err := env.db.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_log WHERE action = 'user.update' AND target = $1`, target).Scan(&n); err != nil {
			t.Fatalf("count update audits: %v", err)
		}
		return n
	}

	// An admin may demote a live_ops user.
	status, body := adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+liveOpsID, adminTok, `{"roles":["viewer"]}`)
	if status != http.StatusOK {
		t.Fatalf("demote status = %d, want 200 (body %s)", status, body)
	}
	if got := decodeJSON[testAdminUser](t, body); len(got.Roles) != 1 || got.Roles[0] != "viewer" {
		t.Errorf("roles = %v, want [viewer]", got.Roles)
	}
	if n := countUpdates(liveOpsID); n != 1 {
		t.Errorf("user.update rows after demote = %d, want 1", n)
	}

	// An admin may not promote to admin.
	status, body = adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+liveOpsID, adminTok, `{"roles":["admin"]}`)
	if status != http.StatusForbidden || decodeAPIError(t, body).Error.Code != "insufficient_role" {
		t.Fatalf("admin promote = %d/%s, want 403 insufficient_role", status, body)
	}

	// Root may promote to admin.
	status, body = adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+liveOpsID, rootTok, `{"roles":["admin"]}`)
	if status != http.StatusOK {
		t.Fatalf("root promote status = %d, want 200 (body %s)", status, body)
	}
	if n := countUpdates(liveOpsID); n != 2 {
		t.Errorf("user.update rows after promote = %d, want 2", n)
	}

	// An admin may not disable another admin.
	status, body = adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+otherAdminID, adminTok, `{"status":"disabled"}`)
	if status != http.StatusForbidden || decodeAPIError(t, body).Error.Code != "insufficient_role" {
		t.Fatalf("admin disables admin = %d/%s, want 403 insufficient_role", status, body)
	}

	// Root may disable an admin, and that admin's live sessions are revoked.
	if _, err := env.db.Pool.Exec(context.Background(),
		`INSERT INTO refresh_session (family_id, user_id, token_hash, expires_at)
		 VALUES (gen_random_uuid(), $1::uuid, $2, now() + interval '1 hour')`,
		otherAdminID, []byte("session-"+otherAdminID)); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	status, body = adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+otherAdminID, rootTok, `{"status":"disabled"}`)
	if status != http.StatusOK {
		t.Fatalf("root disables admin status = %d, want 200 (body %s)", status, body)
	}
	if got := decodeJSON[testAdminUser](t, body); got.Status != "disabled" {
		t.Errorf("status = %q, want disabled", got.Status)
	}
	var live int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_session WHERE user_id = $1::uuid AND revoked_at IS NULL`, otherAdminID).Scan(&live); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if live != 0 {
		t.Errorf("live sessions after disabling the admin = %d, want 0", live)
	}

	// Root is protected from everyone, including another admin.
	status, body = adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+rootID, adminTok, `{"status":"disabled"}`)
	if status != http.StatusForbidden || decodeAPIError(t, body).Error.Code != "root_protected" {
		t.Fatalf("patch root = %d/%s, want 403 root_protected", status, body)
	}

	// And from itself.
	status, body = adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+rootID, rootTok, `{"roles":["admin"]}`)
	if status != http.StatusForbidden || decodeAPIError(t, body).Error.Code != "root_protected" {
		t.Fatalf("root patches itself = %d/%s, want 403 root_protected", status, body)
	}

	// An admin may not change its own account.
	status, body = adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+adminID, adminTok, `{"roles":["viewer"]}`)
	if status != http.StatusForbidden || decodeAPIError(t, body).Error.Code != "self_modification" {
		t.Fatalf("self patch = %d/%s, want 403 self_modification", status, body)
	}

	// An empty change is a validation failure.
	status, body = adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+liveOpsID, adminTok, `{}`)
	if status != http.StatusBadRequest || decodeAPIError(t, body).Error.Code != "validation_failed" {
		t.Fatalf("empty patch = %d/%s, want 400 validation_failed", status, body)
	}

	// Unknown target is 404.
	status, body = adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+uuid.New().String(), adminTok, `{"status":"disabled"}`)
	if status != http.StatusNotFound {
		t.Fatalf("unknown target = %d, want 404 (body %s)", status, body)
	}
}

func TestAdminResetLink(t *testing.T) {
	env := newAdminEnv(t)
	rootID, _, rootTok := env.newRoot(t)
	_, _, adminTok := env.staff(t, "admin")
	targetID, _, _ := env.staff(t, "admin")

	countAudits := func(target string) int {
		var n int
		if err := env.db.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_log WHERE action = 'user.reset_link' AND target = $1`, target).Scan(&n); err != nil {
			t.Fatalf("count reset audits: %v", err)
		}
		return n
	}

	// An admin target needs a root caller.
	status, body := adminCall(t, env.h, http.MethodPost, "/api/admin/users/"+targetID+"/reset", adminTok, "")
	if status != http.StatusForbidden || decodeAPIError(t, body).Error.Code != "insufficient_role" {
		t.Fatalf("non-root reset admin = %d/%s, want 403 insufficient_role", status, body)
	}

	// A root account is protected.
	status, body = adminCall(t, env.h, http.MethodPost, "/api/admin/users/"+rootID+"/reset", rootTok, "")
	if status != http.StatusForbidden || decodeAPIError(t, body).Error.Code != "root_protected" {
		t.Fatalf("reset root = %d/%s, want 403 root_protected", status, body)
	}

	// Root can create a reset for an admin.
	status, body = adminCall(t, env.h, http.MethodPost, "/api/admin/users/"+targetID+"/reset", rootTok, "")
	if status != http.StatusCreated {
		t.Fatalf("reset status = %d, want 201 (body %s)", status, body)
	}
	first := decodeJSON[testResetCreated](t, body)
	if !strings.HasPrefix(first.ResetURL, env.cfg.PublicURL+"/admin/onboard#token=") {
		t.Errorf("reset_url = %q, want the public URL with a token fragment", first.ResetURL)
	}
	firstToken := fragmentToken(t, first.ResetURL)
	wantHash := sha256.Sum256([]byte(firstToken))

	var (
		purpose string
		role    *string
		userID  *string
		hash    []byte
	)
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT token_hash, purpose, role, user_id::text FROM staff_invite WHERE id = $1::uuid`, first.InviteID).
		Scan(&hash, &purpose, &role, &userID); err != nil {
		t.Fatalf("read reset row: %v", err)
	}
	if purpose != "reset" || role != nil || userID == nil || *userID != targetID {
		t.Errorf("reset row = (%s, role %v, user %v), want reset/NULL/%s", purpose, role, userID, targetID)
	}
	if !bytes.Equal(hash, wantHash[:]) {
		t.Errorf("reset token_hash = %x, want sha256 of the fragment token", hash)
	}
	if n := countAudits(first.InviteID); n != 1 {
		t.Errorf("user.reset_link rows = %d, want 1", n)
	}

	// A second reset replaces the first: the earlier row is revoked, exactly one is live.
	status, body = adminCall(t, env.h, http.MethodPost, "/api/admin/users/"+targetID+"/reset", rootTok, "")
	if status != http.StatusCreated {
		t.Fatalf("second reset status = %d, want 201 (body %s)", status, body)
	}
	second := decodeJSON[testResetCreated](t, body)
	if second.InviteID == first.InviteID {
		t.Fatal("the second reset reused the first invite id")
	}
	var firstRevoked bool
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL FROM staff_invite WHERE id = $1::uuid`, first.InviteID).Scan(&firstRevoked); err != nil {
		t.Fatalf("read first reset: %v", err)
	}
	if !firstRevoked {
		t.Error("the earlier pending reset was not revoked")
	}
	var live int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM staff_invite
		 WHERE user_id = $1::uuid AND purpose = 'reset' AND used_at IS NULL AND revoked_at IS NULL`,
		targetID).Scan(&live); err != nil {
		t.Fatalf("count live resets: %v", err)
	}
	if live != 1 {
		t.Errorf("live resets = %d, want 1", live)
	}
}

// apiCodeOrEmpty returns the COM-5 error code for an error response, or "" for a
// success.
func apiCodeOrEmpty(t *testing.T, status int, body string) string {
	t.Helper()
	if status < 400 {
		return ""
	}
	return decodeAPIError(t, body).Error.Code
}

// TestAdminRoleGrantMatrix walks the D3 role-grant rules as one table: every actor
// (root, admin, live_ops, viewer) against every sensitive action, through the real
// HTTP API. The store's DB-sourced authorisation and the middleware's admin gate are
// both in play, so this is the end-to-end permission matrix.
func TestAdminRoleGrantMatrix(t *testing.T) {
	env := newAdminEnv(t)
	rootID, _, rootTok := env.newRoot(t)
	adminID, _, adminTok := env.staff(t, "admin")
	liveOpsID, _, liveOpsTok := env.staff(t, "live_ops")
	viewerID, _, viewerTok := env.staff(t, "viewer")

	type actor struct{ name, id, tok string }
	actors := []actor{
		{"root", rootID, rootTok},
		{"admin", adminID, adminTok},
		{"live_ops", liveOpsID, liveOpsTok},
		{"viewer", viewerID, viewerTok},
	}

	seq := 0
	nextEmail := func() string {
		seq++
		return uniqueLoginEmail(fmt.Sprintf("matrix%d", seq))
	}
	// fresh creates a new target account with the given roles for the action at hand.
	fresh := func(roles ...string) string {
		id, _, _ := env.staff(t, roles...)
		return id
	}

	type outcome struct {
		status int
		code   string
	}
	type action struct {
		name string
		run  func(t *testing.T, a actor) (int, string)
	}

	invite := func(role string) action {
		return action{
			name: "invite " + role,
			run: func(t *testing.T, a actor) (int, string) {
				body := fmt.Sprintf(`{"email":%q,"name":"Matrix","role":%q}`, nextEmail(), role)
				status, raw := adminCall(t, env.h, http.MethodPost, "/api/admin/users", a.tok, body)
				return status, apiCodeOrEmpty(t, status, raw)
			},
		}
	}
	patch := func(name string, target func(a actor) string, roleField, statusField string) action {
		return action{
			name: name,
			run: func(t *testing.T, a actor) (int, string) {
				var fields []string
				if roleField != "" {
					fields = append(fields, roleField)
				}
				if statusField != "" {
					fields = append(fields, statusField)
				}
				body := "{" + strings.Join(fields, ",") + "}"
				status, raw := adminCall(t, env.h, http.MethodPatch, "/api/admin/users/"+target(a), a.tok, body)
				return status, apiCodeOrEmpty(t, status, raw)
			},
		}
	}

	actions := []action{
		invite("viewer"),
		invite("live_ops"),
		invite("admin"),
		patch("PATCH roles to admin", func(a actor) string { return fresh("live_ops") }, `"roles":["admin"]`, ""),
		patch("PATCH own roles", func(a actor) string { return a.id }, `"roles":["viewer"]`, ""),
		patch("disable root", func(a actor) string { return rootID }, "", `"status":"disabled"`),
		patch("disable self", func(a actor) string { return a.id }, "", `"status":"disabled"`),
		patch("disable other admin", func(a actor) string { return fresh("admin") }, "", `"status":"disabled"`),
	}

	nonAdmin := outcome{http.StatusForbidden, "insufficient_role"}
	want := map[string]map[string]outcome{
		"root": {
			"invite viewer":        {http.StatusCreated, ""},
			"invite live_ops":      {http.StatusCreated, ""},
			"invite admin":         {http.StatusCreated, ""},
			"PATCH roles to admin": {http.StatusOK, ""},
			"PATCH own roles":      {http.StatusForbidden, "root_protected"},
			"disable root":         {http.StatusForbidden, "root_protected"},
			"disable self":         {http.StatusForbidden, "root_protected"},
			"disable other admin":  {http.StatusOK, ""},
		},
		"admin": {
			"invite viewer":        {http.StatusCreated, ""},
			"invite live_ops":      {http.StatusCreated, ""},
			"invite admin":         {http.StatusForbidden, "insufficient_role"},
			"PATCH roles to admin": {http.StatusForbidden, "insufficient_role"},
			"PATCH own roles":      {http.StatusForbidden, "self_modification"},
			"disable root":         {http.StatusForbidden, "root_protected"},
			"disable self":         {http.StatusForbidden, "self_modification"},
			"disable other admin":  {http.StatusForbidden, "insufficient_role"},
		},
		"live_ops": {
			"invite viewer":        nonAdmin,
			"invite live_ops":      nonAdmin,
			"invite admin":         nonAdmin,
			"PATCH roles to admin": nonAdmin,
			"PATCH own roles":      nonAdmin,
			"disable root":         nonAdmin,
			"disable self":         nonAdmin,
			"disable other admin":  nonAdmin,
		},
		"viewer": {
			"invite viewer":        nonAdmin,
			"invite live_ops":      nonAdmin,
			"invite admin":         nonAdmin,
			"PATCH roles to admin": nonAdmin,
			"PATCH own roles":      nonAdmin,
			"disable root":         nonAdmin,
			"disable self":         nonAdmin,
			"disable other admin":  nonAdmin,
		},
	}

	for _, a := range actors {
		for _, act := range actions {
			t.Run(a.name+"/"+act.name, func(t *testing.T) {
				expect := want[a.name][act.name]
				status, code := act.run(t, a)
				if status != expect.status {
					t.Fatalf("status = %d, want %d (code %q)", status, expect.status, code)
				}
				if code != expect.code {
					t.Errorf("code = %q, want %q", code, expect.code)
				}
			})
		}
	}
}
