package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/events"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

const partyID = "018f4a3e-0000-7000-8000-000000000001"

type fakeAdmin struct {
	profiles  []store.Profile
	lookedUp  string
	disc      *int
	party     *store.Party
	disbanded string
	actor     store.Entry
	audit     []store.AuditRecord
	next      *int64
	query     store.AuditQuery
	limit     int
}

func (f *fakeAdmin) LookupPlayers(_ context.Context, name string, d *int) ([]store.Profile, error) {
	f.lookedUp, f.disc = name, d
	return f.profiles, nil
}
func (f *fakeAdmin) GetProfile(_ context.Context, id uuid.UUID) (store.Profile, error) {
	for _, p := range f.profiles {
		if p.PlayerID == id {
			return p, nil
		}
	}
	return store.Profile{}, store.ErrProfileNotFound
}
func (f *fakeAdmin) GetParty(context.Context, string) (*store.Party, error) {
	if f.party == nil {
		return nil, store.ErrNotInParty
	}
	return f.party, nil
}
func (f *fakeAdmin) ForceDisband(_ context.Context, id string, actor store.Entry) (*store.Party, []store.Notice, error) {
	if f.party == nil || f.party.ID != id {
		return nil, nil, store.ErrPartyNotFound
	}
	f.disbanded, f.actor = id, actor
	var n []store.Notice
	for _, m := range f.party.Members {
		n = append(n, store.Notice{PlayerID: m.PlayerID, Type: events.TypePartyDisbanded})
	}
	return f.party, n, nil
}
func (f *fakeAdmin) ListAudit(_ context.Context, q store.AuditQuery, limit int) ([]store.AuditRecord, *int64, error) {
	f.query, f.limit = q, limit
	return f.audit, f.next, nil
}

// staffCall sends a staff request with a staff identity carrying name.
func staffCall(t *testing.T, h *Handlers, method, pattern, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(method+" "+StaffPrefix+pattern, h.For(Route{Method: method, Path: StaffPrefix + pattern, Group: GroupStaff}))
	req := httptest.NewRequest(method, StaffPrefix+path, strings.NewReader(body))
	req = req.WithContext(auth.WithIdentity(req.Context(),
		&auth.Identity{Subject: "staff-7", Name: "Grace Hopper", Roles: []string{"live_ops"}}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestLookupPlayers(t *testing.T) {
	fa := &fakeAdmin{profiles: []store.Profile{{PlayerID: testPlayer, DisplayName: "Tanuki", Discriminator: 4417}}}
	h := &Handlers{Admin: fa}
	rec := staffCall(t, h, http.MethodGet, "/players", "/players?name=tanuki&discriminator=4417", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup = %d %s", rec.Code, rec.Body)
	}
	var got struct{ Players []map[string]any }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Players) != 1 || got.Players[0]["discriminator"] != float64(4417) || fa.lookedUp != "tanuki" || *fa.disc != 4417 {
		t.Errorf("players = %v, looked up %q %v", got.Players, fa.lookedUp, fa.disc)
	}
	for _, q := range []string{"", "?name=", "?name=x&discriminator=0", "?name=x&discriminator=abc"} {
		if rec := staffCall(t, h, http.MethodGet, "/players", "/players"+q, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("GET /players%s = %d, want 400", q, rec.Code)
		}
	}
}

func TestGetPlayerShowsProfilePresenceAndParty(t *testing.T) {
	fa := &fakeAdmin{
		profiles: []store.Profile{{PlayerID: testPlayer, DisplayName: "Tanuki", Discriminator: 4417}},
		party:    sampleParty(),
	}
	h := &Handlers{Admin: fa, Presence: &fakePresence{statuses: map[string]string{testPlayer.String(): "in_menus"}}}
	rec := staffCall(t, h, http.MethodGet, "/players/{id}", "/players/"+testPlayer.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /players/{id} = %d %s", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	party, _ := got["party"].(map[string]any)
	if got["display_name"] != "Tanuki" || got["status"] != "in_menus" || party == nil || party["party_id"] != partyID {
		t.Errorf("body = %v", got)
	}

	fa.party = nil
	rec = staffCall(t, h, http.MethodGet, "/players/{id}", "/players/"+testPlayer.String(), "")
	if !strings.Contains(rec.Body.String(), `"party":null`) {
		t.Errorf("a player in no party = %s, want party null", rec.Body)
	}
	if rec := staffCall(t, h, http.MethodGet, "/players/{id}", "/players/"+otherPlayer, ""); rec.Code != http.StatusNotFound || errCode(t, rec) != "player_not_found" {
		t.Errorf("unknown player = %d %s", rec.Code, rec.Body)
	}
}

// TestForceDisbandIsAuditedAsTheCaller is SE-7's criterion from the handler's side: the
// audit row names the staff member by id and name, with the reason, and every member is
// told.
func TestForceDisbandIsAuditedAsTheCaller(t *testing.T) {
	fa := &fakeAdmin{party: sampleParty()}
	pub := &recordingPublisher{}
	h := &Handlers{Admin: fa, Publisher: pub}

	rec := staffCall(t, h, http.MethodPost, "/parties/{party_id}/disband", "/parties/"+partyID+"/disband", `{"reason":"griefing report 812"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disband = %d %s", rec.Code, rec.Body)
	}
	if fa.disbanded != partyID || fa.actor.ActorID != "staff-7" || fa.actor.ActorName != "Grace Hopper" || fa.actor.Details["reason"] != "griefing report 812" {
		t.Errorf("disbanded %q as %+v", fa.disbanded, fa.actor)
	}
	if len(pub.published) != 2 || pub.published[0].Type != events.TypePartyDisbanded {
		t.Errorf("published %+v, want party.disbanded to both members", pub.published)
	}

	// No body is fine; an unknown party is 404.
	fa.party = sampleParty()
	if rec := staffCall(t, h, http.MethodPost, "/parties/{party_id}/disband", "/parties/"+partyID+"/disband", ""); rec.Code != http.StatusNoContent {
		t.Errorf("disband without a body = %d %s", rec.Code, rec.Body)
	}
	if rec := staffCall(t, h, http.MethodPost, "/parties/{party_id}/disband", "/parties/"+otherPlayer+"/disband", ""); rec.Code != http.StatusNotFound || errCode(t, rec) != "party_not_found" {
		t.Errorf("unknown party = %d %s", rec.Code, rec.Body)
	}
	long := `{"reason":"` + strings.Repeat("x", 501) + `"}`
	if rec := staffCall(t, h, http.MethodPost, "/parties/{party_id}/disband", "/parties/"+partyID+"/disband", long); rec.Code != http.StatusBadRequest {
		t.Errorf("a 501-character reason = %d, want 400", rec.Code)
	}
}

// TestAuditFeedIsConfigsShape: the Dashboard decodes Session's feed with the decoder it
// uses for Config's, which refuses unknown fields, so the shape must match exactly.
func TestAuditFeedIsConfigsShape(t *testing.T) {
	next := int64(41)
	fa := &fakeAdmin{
		audit: []store.AuditRecord{{ID: 42, At: time.Unix(1_800_000_000, 0).UTC(), ActorID: "staff-7", ActorName: "Grace Hopper",
			Action: store.ActionDisbandForced, Target: partyID, Details: json.RawMessage(`{"state":"in_game"}`)}},
		next: &next,
	}
	h := &Handlers{Admin: fa}
	rec := staffCall(t, h, http.MethodGet, "/audit", "/audit?limit=1&actor=Grace+Hopper&from=2026-09-01T00:00:00Z", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body)
	}
	var page struct {
		Entries []struct {
			ID        int64           `json:"id"`
			At        time.Time       `json:"at"`
			ActorID   string          `json:"actor_id"`
			ActorName string          `json:"actor_name"`
			Source    string          `json:"source"`
			Action    string          `json:"action"`
			Target    string          `json:"target"`
			Details   json.RawMessage `json:"details"`
		} `json:"entries"`
		NextCursor *string `json:"next_cursor"`
	}
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&page); err != nil {
		t.Fatalf("the Dashboard's decoder refuses the page: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Source != "session" || page.Entries[0].Action != "party.disband_forced" {
		t.Errorf("entries = %+v", page.Entries)
	}
	if fa.limit != 1 || *fa.query.Actor != "Grace Hopper" || fa.query.From == nil {
		t.Errorf("query = %+v limit %d", fa.query, fa.limit)
	}

	// The cursor round-trips.
	if page.NextCursor == nil {
		t.Fatal("no next_cursor on a full page")
	}
	staffCall(t, h, http.MethodGet, "/audit", "/audit?cursor="+*page.NextCursor, "")
	if fa.query.Before == nil || *fa.query.Before != 41 {
		t.Errorf("cursor decoded to %v, want 41", fa.query.Before)
	}
	for _, q := range []string{"?limit=0", "?limit=201", "?cursor=!!", "?from=yesterday"} {
		if rec := staffCall(t, h, http.MethodGet, "/audit", "/audit"+q, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("GET /audit%s = %d, want 400", q, rec.Code)
		}
	}
}
