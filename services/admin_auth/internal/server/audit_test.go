package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// auditEntryWire is the shared entry shape the Dashboard merges across services.
type auditEntryWire struct {
	ID        int64          `json:"id"`
	At        time.Time      `json:"at"`
	ActorID   string         `json:"actor_id"`
	ActorName string         `json:"actor_name"`
	Source    string         `json:"source"`
	Action    string         `json:"action"`
	Target    string         `json:"target"`
	Details   map[string]any `json:"details"`
}

type auditPageWire struct {
	Entries    []auditEntryWire `json:"entries"`
	NextCursor *string          `json:"next_cursor"`
}

// getAudit calls GET /admin-auth/audit with the given raw query and bearer.
func getAudit(t *testing.T, h *harness, tok, query string) (int, auditPageWire, string) {
	t.Helper()

	path := "/admin-auth/audit"
	if query != "" {
		path += "?" + query
	}
	status, body := adminCall(t, h, http.MethodGet, path, tok, "")
	var page auditPageWire
	if status == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatalf("audit body %q is not JSON: %v", body, err)
		}
	}
	return status, page, body
}

// loginForAudit performs one successful login and returns the resulting access token
// and user id, so a test can both create a login.success entry and read the feed as
// that viewer.
func loginForAudit(t *testing.T, env *adminEnv) (id, email, tok string) {
	t.Helper()

	id, email, _ = env.staff(t, "viewer")
	status, _, body := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, "password-"+email))
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body %s)", status, body)
	}
	return id, email, decodeLoginBody(t, body).AccessToken
}

func TestAuditFeedCarriesTheSharedShape(t *testing.T) {
	env := newAdminEnv(t)

	viewerID, _, viewerTok := loginForAudit(t, env)
	adminID, _, adminTok := env.staff(t, "admin")

	inviteEmail := uniqueLoginEmail("auditinvite")
	status, body := adminCall(t, env.h, http.MethodPost, "/api/admin/users", adminTok,
		fmt.Sprintf(`{"email":%q,"name":"Audit Invitee","role":"viewer"}`, inviteEmail))
	if status != http.StatusCreated {
		t.Fatalf("invite status = %d, want 201 (body %s)", status, body)
	}

	// The viewer reads the feed: the minimum role is viewer, not admin.
	status, page, body := getAudit(t, env.h, viewerTok, "action=login.success&actor="+url.QueryEscape(viewerID))
	if status != http.StatusOK {
		t.Fatalf("audit status = %d, want 200 (body %s)", status, body)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("login.success entries = %d, want 1 (body %s)", len(page.Entries), body)
	}
	login := page.Entries[0]
	if login.Source != "admin-auth" {
		t.Errorf("source = %q, want admin-auth", login.Source)
	}
	if login.ActorID != viewerID {
		t.Errorf("actor_id = %q, want %s", login.ActorID, viewerID)
	}
	if login.ActorName == "" || login.Target != viewerID || login.Action != "login.success" {
		t.Errorf("login entry = %+v, want the actor, target and action filled in", login)
	}
	if login.ID < 1 {
		t.Errorf("id = %d, want a positive bigserial", login.ID)
	}
	if login.At.IsZero() {
		t.Error("at is zero")
	}
	if login.Details == nil {
		t.Error("details is null, want an object")
	}

	// The invite entry, filtered to the admin who created it.
	status, page, body = getAudit(t, env.h, viewerTok, "action=user.invite&actor="+url.QueryEscape(adminID))
	if status != http.StatusOK {
		t.Fatalf("audit invite status = %d, want 200 (body %s)", status, body)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("user.invite entries = %d, want 1 (body %s)", len(page.Entries), body)
	}
	invite := page.Entries[0]
	if invite.Source != "admin-auth" || invite.Action != "user.invite" || invite.ActorID != adminID {
		t.Errorf("invite entry = %+v, want source admin-auth, action user.invite, actor %s", invite, adminID)
	}
	if got, _ := invite.Details["email"].(string); got != inviteEmail {
		t.Errorf("invite details.email = %v, want %s", invite.Details["email"], inviteEmail)
	}
}

func TestAuditPagingWalksEveryRowWithoutDuplicates(t *testing.T) {
	env := newAdminEnv(t)

	// Three successful logins make three login.success rows for one actor.
	id, email, _ := env.staff(t, "viewer")
	for i := 0; i < 3; i++ {
		status, _, body := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, "password-"+email))
		if status != http.StatusOK {
			t.Fatalf("login %d status = %d, want 200 (body %s)", i, status, body)
		}
	}
	_, _, tok := loginForAudit(t, env) // a token to read the feed

	seen := map[int64]bool{}
	pages := 0
	cursor := ""
	for {
		query := "action=login.success&actor=" + url.QueryEscape(id) + "&limit=1"
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		status, page, body := getAudit(t, env.h, tok, query)
		if status != http.StatusOK {
			t.Fatalf("page %d status = %d, want 200 (body %s)", pages, status, body)
		}
		for _, e := range page.Entries {
			if seen[e.ID] {
				t.Errorf("entry %d appeared twice across pages", e.ID)
			}
			seen[e.ID] = true
		}
		pages++
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != 3 {
		t.Errorf("walked %d rows, want 3", len(seen))
	}
}

func TestAuditFilters(t *testing.T) {
	env := newAdminEnv(t)
	viewerID, _, readerTok := loginForAudit(t, env)

	status, page, body := getAudit(t, env.h, readerTok, "action=login.success&actor="+url.QueryEscape(viewerID))
	if status != http.StatusOK || len(page.Entries) == 0 {
		t.Fatalf("baseline audit = %d/%d entries (body %s)", status, len(page.Entries), body)
	}
	at := page.Entries[0].At

	// from and to are inclusive, so the entry's own instant selects it.
	query := "action=login.success&actor=" + url.QueryEscape(viewerID) +
		"&from=" + url.QueryEscape(at.Format(time.RFC3339Nano)) +
		"&to=" + url.QueryEscape(at.Format(time.RFC3339Nano))
	status, page, body = getAudit(t, env.h, readerTok, query)
	if status != http.StatusOK {
		t.Fatalf("from/to audit status = %d, want 200 (body %s)", status, body)
	}
	if len(page.Entries) != 1 {
		t.Errorf("from/to matched %d entries, want 1 (body %s)", len(page.Entries), body)
	}

	// A window in the future matches nothing.
	future := at.Add(24 * time.Hour).Format(time.RFC3339)
	status, page, _ = getAudit(t, env.h, readerTok, "from="+url.QueryEscape(future))
	if status != http.StatusOK || len(page.Entries) != 0 {
		t.Errorf("future from returned %d entries (status %d), want none", len(page.Entries), status)
	}

	// Bad timestamps are a 400.
	for _, q := range []string{"from=not-a-time", "to=2020-13-01T00:00:00Z"} {
		if status, _, _ := getAudit(t, env.h, readerTok, q); status != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", q, status)
		}
	}
}

func TestAuditRejectsMalformedPaging(t *testing.T) {
	env := newAdminEnv(t)
	_, _, tok := loginForAudit(t, env)

	badCursor := base64.RawURLEncoding.EncodeToString([]byte(`{"before":0}`))
	cases := []string{
		"limit=0", "limit=201", "limit=abc", "limit=-1",
		"cursor=!!!not-base64", "cursor=" + badCursor,
	}
	for _, q := range cases {
		status, _, body := getAudit(t, env.h, tok, q)
		if status != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400 (body %s)", q, status, body)
			continue
		}
		if got := decodeAPIError(t, body); got.Error.Code != "validation_failed" {
			t.Errorf("%s code = %q, want validation_failed", q, got.Error.Code)
		}
	}
}

func TestAuditRejections(t *testing.T) {
	env := newAdminEnv(t)
	id, _, tok := loginForAudit(t, env)

	// No bearer.
	status, _, body := getAudit(t, env.h, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401 (body %s)", status, body)
	}
	if got := decodeAPIError(t, body); got.Error.Code != "missing_token" {
		t.Errorf("no token code = %q, want missing_token", got.Error.Code)
	}

	// A token whose account is disabled stops resolving.
	if _, err := env.db.Pool.Exec(context.Background(),
		`UPDATE staff_user SET status = 'disabled' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("disable staff: %v", err)
	}
	status, _, body = getAudit(t, env.h, tok, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("disabled token status = %d, want 401 (body %s)", status, body)
	}
	if got := decodeAPIError(t, body); got.Error.Code != "invalid_token" {
		t.Errorf("disabled token code = %q, want invalid_token", got.Error.Code)
	}
}
