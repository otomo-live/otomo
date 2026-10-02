package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/rules"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

// fakeProfiles is an in-memory ProfileStore. It records what the handlers pass it, and a
// test can make any call fail.
type fakeProfiles struct {
	mu        sync.Mutex
	profiles  map[uuid.UUID]store.Profile
	renameErr error

	lastName     string
	lastCooldown time.Duration
}

func newFakeProfiles() *fakeProfiles {
	return &fakeProfiles{profiles: map[uuid.UUID]store.Profile{}}
}

func (f *fakeProfiles) InitProfile(_ context.Context, id uuid.UUID, provisional func() string) (store.Profile, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.profiles[id]; ok {
		return p, false, nil
	}
	p := store.Profile{PlayerID: id, DisplayName: provisional(), Discriminator: 4417}
	f.profiles[id] = p
	return p, true, nil
}

func (f *fakeProfiles) GetProfile(_ context.Context, id uuid.UUID) (store.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.profiles[id]
	if !ok {
		return store.Profile{}, store.ErrProfileNotFound
	}
	return p, nil
}

func (f *fakeProfiles) RenameProfile(_ context.Context, id uuid.UUID, name string, cooldown time.Duration) (store.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastName, f.lastCooldown = name, cooldown
	if f.renameErr != nil {
		return store.Profile{}, f.renameErr
	}
	p, ok := f.profiles[id]
	if !ok {
		return store.Profile{}, store.ErrProfileNotFound
	}
	p.DisplayName = name
	f.profiles[id] = p
	return p, nil
}

var testPlayer = uuid.MustParse("018f4a3e-1c2d-7abc-8def-0123456789ab")

// call runs one request through the handler For picks for method and path, as the guard
// would after accepting a token for testPlayer.
func call(t *testing.T, h *Handlers, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rt := Route{Method: method, Path: PlayerPrefix + path, Group: GroupPlayer}
	req := httptest.NewRequest(method, PlayerPrefix+path, strings.NewReader(body))
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{Subject: testPlayer.String()}))
	rec := httptest.NewRecorder()
	h.For(rt).ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

func TestInitCreatesThenReturnsTheSameProfile(t *testing.T) {
	h := &Handlers{Profiles: newFakeProfiles()}

	first := call(t, h, http.MethodPost, "/me/init", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first init = %d %s", first.Code, first.Body)
	}
	// The SDK decodes discriminator as an int (ISessionClient.cs), so it must be a JSON
	// number, never a string.
	var got map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["player_id"] != testPlayer.String() {
		t.Errorf("player_id = %v", got["player_id"])
	}
	if _, ok := got["discriminator"].(float64); !ok {
		t.Errorf("discriminator = %#v, want a JSON number", got["discriminator"])
	}
	name, _ := got["display_name"].(string)
	if !strings.HasPrefix(name, provisionalPrefix) || len(name) != len(provisionalPrefix)+4 {
		t.Errorf("display_name = %q, want %s and four digits", name, provisionalPrefix)
	}

	second := call(t, h, http.MethodPost, "/me/init", "")
	if second.Code != http.StatusOK || second.Body.String() != first.Body.String() {
		t.Errorf("second init = %d %s, want 200 and the same body", second.Code, second.Body)
	}
}

func TestGetProfile(t *testing.T) {
	h := &Handlers{Profiles: newFakeProfiles()}

	if rec := call(t, h, http.MethodGet, "/me", ""); rec.Code != http.StatusNotFound || errCode(t, rec) != "profile_not_found" {
		t.Errorf("GET /me before init = %d %s, want 404 profile_not_found", rec.Code, rec.Body)
	}
	call(t, h, http.MethodPost, "/me/init", "")
	if rec := call(t, h, http.MethodGet, "/me", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /me after init = %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestRenameValidatesTheName(t *testing.T) {
	fake := newFakeProfiles()
	h := &Handlers{Profiles: fake}
	call(t, h, http.MethodPost, "/me/init", "")

	for _, tt := range []struct {
		body, code string
		status     int
	}{
		{`{"display_name":"ab"}`, "invalid_display_name", http.StatusBadRequest},
		{`{"display_name":"no spaces"}`, "invalid_display_name", http.StatusBadRequest},
		{`{"display_name":"admin"}`, "invalid_display_name", http.StatusBadRequest},
		{`{}`, "invalid_display_name", http.StatusBadRequest},
		{`{"display_name":"Tanuki","extra":1}`, "invalid_body", http.StatusBadRequest},
		{`{"display_name":"Tanuki"}{}`, "invalid_body", http.StatusBadRequest},
		{`not json`, "invalid_body", http.StatusBadRequest},
		{`{"display_name":"` + strings.Repeat("a", 2000) + `"}`, "body_too_large", http.StatusRequestEntityTooLarge},
	} {
		rec := call(t, h, http.MethodPatch, "/me", tt.body)
		if rec.Code != tt.status || errCode(t, rec) != tt.code {
			t.Errorf("PATCH /me %.40s = %d %s, want %d %s", tt.body, rec.Code, rec.Body, tt.status, tt.code)
		}
	}
	if fake.lastName != "" {
		t.Errorf("an invalid name reached the store: %q", fake.lastName)
	}

	rec := call(t, h, http.MethodPatch, "/me", `{"display_name":"  Tanuki  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid rename = %d %s", rec.Code, rec.Body)
	}
	if fake.lastName != "Tanuki" || fake.lastCooldown != 24*time.Hour {
		t.Errorf("store got name %q cooldown %v, want the trimmed name and 24h", fake.lastName, fake.lastCooldown)
	}
}

func TestRenameTooSoonIs429WithRetryAfter(t *testing.T) {
	fake := newFakeProfiles()
	h := &Handlers{Profiles: fake}
	call(t, h, http.MethodPost, "/me/init", "")

	fake.renameErr = &store.RenameTooSoonError{RetryAt: time.Now().Add(90 * time.Minute)}
	rec := call(t, h, http.MethodPatch, "/me", `{"display_name":"Tanuki"}`)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != "rate_limit_exceeded" {
		t.Fatalf("rename too soon = %d %s, want 429 rate_limit_exceeded", rec.Code, rec.Body)
	}
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || secs < 5390 || secs > 5400 {
		t.Errorf("Retry-After = %q, want about 5400", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "once every 24 hours") {
		t.Errorf("message does not state the cooldown: %s", rec.Body)
	}
}

func TestRenameStoreOutcomes(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrProfileNotFound, http.StatusNotFound, "profile_not_found"},
		{store.ErrNameUnavailable, http.StatusConflict, "name_unavailable"},
		{errors.New("connection reset"), http.StatusInternalServerError, "internal_error"},
	} {
		fake := newFakeProfiles()
		fake.renameErr = tt.err
		rec := call(t, &Handlers{Profiles: fake}, http.MethodPatch, "/me", `{"display_name":"Tanuki"}`)
		if rec.Code != tt.status || errCode(t, rec) != tt.code {
			t.Errorf("store error %v = %d %s, want %d %s", tt.err, rec.Code, rec.Body, tt.status, tt.code)
		}
		if strings.Contains(rec.Body.String(), "connection reset") {
			t.Error("a store error leaked into the response")
		}
	}
}

// TestRenameReadsTheRulesPerRequest: the handler takes its name rules and cooldown from
// the Source on every call, which is what lets LB-1 swap in live rules.
func TestRenameReadsTheRulesPerRequest(t *testing.T) {
	strict := rules.Defaults()
	strict.Names.MinLength = 8
	strict.Names.RenameCooldownHours = 1

	fake := newFakeProfiles()
	h := &Handlers{Profiles: fake, Rules: rules.Static{R: strict}}
	call(t, h, http.MethodPost, "/me/init", "")

	if rec := call(t, h, http.MethodPatch, "/me", `{"display_name":"Tanuki"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("6-character name under min_length 8 = %d, want 400", rec.Code)
	}
	if rec := call(t, h, http.MethodPatch, "/me", `{"display_name":"TanukiTanuki"}`); rec.Code != http.StatusOK {
		t.Fatalf("12-character name = %d %s, want 200", rec.Code, rec.Body)
	}
	if fake.lastCooldown != time.Hour {
		t.Errorf("cooldown = %v, want the source's 1h", fake.lastCooldown)
	}
}

func TestMissingStoreFailsClosed(t *testing.T) {
	h := &Handlers{}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/me/init", ""},
		{http.MethodGet, "/me", ""},
		{http.MethodPatch, "/me", `{"display_name":"Tanuki"}`},
	} {
		if rec := call(t, h, c.method, c.path, c.body); rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s with no store = %d, want 500", c.method, c.path, rec.Code)
		}
	}
}

// TestEveryRouteHasAHandler: since SE-7 no route in the table is left at its 501
// placeholder, and nil Handlers still answers 501 everywhere, which the server's guard
// tests rely on.
func TestEveryRouteHasAHandler(t *testing.T) {
	impl := (&Handlers{}).implemented()
	for _, rt := range Routes() {
		if _, ok := impl[rt.Pattern()]; !ok {
			t.Errorf("%s has no handler", rt.Pattern())
		}
	}
	var nilHandlers *Handlers
	if rec := call(t, nilHandlers, http.MethodGet, "/me", ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /me with nil Handlers = %d, want 501", rec.Code)
	}
}
