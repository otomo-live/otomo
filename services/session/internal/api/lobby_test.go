package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/otomo-live/otomo/services/session/internal/rules"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

// LB-2 through the handlers. The rules are the seeded defaults: expedition in
// {expedition_1} and difficulty in {normal, hard}.

func settingsCall(t *testing.T, h *Handlers, body string) (int, string) {
	t.Helper()
	rec := partyCall(t, h, t.Context(), http.MethodPatch, "/party/settings", "/party/settings", body)
	code := ""
	if rec.Code >= 400 {
		code = errCode(t, rec)
	}
	return rec.Code, code
}

// TestSettingsFollowTheRules is doc 14 §2: only listed keys, only allowed values, flat
// string values, and a revision.
func TestSettingsFollowTheRules(t *testing.T) {
	fake := &fakeParties{party: sampleParty()}
	h := &Handlers{Parties: fake}

	for _, tt := range []struct {
		body, code string
		status     int
	}{
		{`{"settings":{"difficulty":"nightmare"},"revision":3}`, "invalid_settings", 400},
		{`{"settings":{"weather":"rain"},"revision":3}`, "invalid_settings", 400},
		{`{"settings":{"difficulty":2},"revision":3}`, "invalid_body", 400},
		{`{"settings":{"difficulty":{"level":"hard"}},"revision":3}`, "invalid_body", 400},
		{`{"settings":{"difficulty":"hard"}}`, "revision_required", 400},
		{`{"revision":3}`, "invalid_settings", 400},
	} {
		if status, code := settingsCall(t, h, tt.body); status != tt.status || code != tt.code {
			t.Errorf("PATCH %s = %d %s, want %d %s", tt.body, status, code, tt.status, tt.code)
		}
	}
	if fake.gotSettings != nil {
		t.Fatalf("an invalid request reached the store: %v", fake.gotSettings)
	}

	status, _ := settingsCall(t, h, `{"settings":{"difficulty":"hard"},"revision":3}`)
	if status != http.StatusOK || fake.gotSettings["difficulty"] != "hard" || len(fake.gotSettings) != 1 || fake.gotRevision != 3 {
		t.Errorf("valid PATCH = %d, store got %v at revision %d", status, fake.gotSettings, fake.gotRevision)
	}
}

// TestSettingsUseTheRulesInForce: the allowed values are read per request.
func TestSettingsUseTheRulesInForce(t *testing.T) {
	r := rules.Defaults()
	r.Lobby.Settings["difficulty"] = rules.Setting{Allowed: []string{"normal", "hard", "nightmare"}, Default: "normal"}
	fake := &fakeParties{party: sampleParty()}
	h := &Handlers{Parties: fake, Rules: rules.Static{R: r}}
	if status, _ := settingsCall(t, h, `{"settings":{"difficulty":"nightmare"},"revision":3}`); status != http.StatusOK {
		t.Errorf("a value the live rules allow = %d, want 200", status)
	}
}

func TestSettingsStoreOutcomes(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrNotLeader, http.StatusForbidden, "not_leader"},
		{store.ErrPartyLocked, http.StatusConflict, "party_locked"},
		{&store.RevisionMismatchError{Current: 9}, http.StatusConflict, "revision_mismatch"},
		{store.ErrNotInParty, http.StatusNotFound, "not_in_party"},
	} {
		h := &Handlers{Parties: &fakeParties{err: tt.err}}
		if status, code := settingsCall(t, h, `{"settings":{"difficulty":"hard"},"revision":3}`); status != tt.status || code != tt.code {
			t.Errorf("%v = %d %s, want %d %s", tt.err, status, code, tt.status, tt.code)
		}
	}
}

func TestReady(t *testing.T) {
	fake := &fakeParties{party: sampleParty()}
	h := &Handlers{Parties: fake}

	for _, body := range []string{`{}`, `{"ready":"yes"}`} {
		if rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/ready", "/party/ready", body); rec.Code != http.StatusBadRequest {
			t.Errorf("POST /party/ready %s = %d, want 400", body, rec.Code)
		}
	}
	rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/ready", "/party/ready", `{"ready":true}`)
	if rec.Code != http.StatusOK || fake.gotReady == nil || !*fake.gotReady {
		t.Errorf("ready = %d, store got %v", rec.Code, fake.gotReady)
	}

	fake.err = store.ErrPartyLocked
	if rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/ready", "/party/ready", `{"ready":false}`); rec.Code != http.StatusConflict || errCode(t, rec) != "party_locked" {
		t.Errorf("ready while locked = %d %s, want 409 party_locked", rec.Code, rec.Body)
	}
}

// TestANewLobbyStartsWithEveryDefault: doc 14 §2.
func TestANewLobbyStartsWithEveryDefault(t *testing.T) {
	fake := &fakeParties{party: sampleParty()}
	h := &Handlers{Parties: fake}
	partyCall(t, h, t.Context(), http.MethodPost, "/party", "/party", "")
	if fake.gotSettings["expedition"] != "expedition_1" || fake.gotSettings["difficulty"] != "normal" || len(fake.gotSettings) != 2 {
		t.Errorf("new lobby settings = %v, want every default", fake.gotSettings)
	}
}

func TestGetPartyShowsTheLobby(t *testing.T) {
	p := sampleParty()
	p.State = store.StateForming
	p.Settings = map[string]string{"difficulty": "hard"}
	p.Members[1].Ready = true
	h := &Handlers{Parties: &fakeParties{party: p}}

	rec := partyCall(t, h, t.Context(), http.MethodGet, "/party", "/party", "")
	var got struct {
		State    string            `json:"state"`
		Settings map[string]string `json:"settings"`
		Members  []struct {
			Ready bool `json:"ready"`
		} `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "forming" || got.Settings["difficulty"] != "hard" || got.Members[0].Ready || !got.Members[1].Ready {
		t.Errorf("GET /party = %s", rec.Body)
	}
}

// TestLockedPartyRefusesChanges: every roster and lobby change maps party_locked to 409.
func TestLockedPartyRefusesChanges(t *testing.T) {
	h := &Handlers{Parties: &fakeParties{err: store.ErrPartyLocked}}
	for _, c := range []struct{ method, pattern, path, body string }{
		{http.MethodPost, "/party/invites", "/party/invites", `{"player_id":"` + otherPlayer + `"}`},
		{http.MethodPost, "/party/invites/{invite_id}/accept", "/party/invites/018f4a3e-0000-7000-8000-000000000009/accept", ""},
		{http.MethodPost, "/party/kick/{player_id}", "/party/kick/" + otherPlayer, `{"revision":3}`},
		{http.MethodPost, "/party/promote/{player_id}", "/party/promote/" + otherPlayer, `{"revision":3}`},
	} {
		rec := partyCall(t, h, t.Context(), c.method, c.pattern, c.path, c.body)
		if rec.Code != http.StatusConflict || errCode(t, rec) != "party_locked" {
			t.Errorf("%s %s while locked = %d %s, want 409 party_locked", c.method, c.path, rec.Code, rec.Body)
		}
	}
}
