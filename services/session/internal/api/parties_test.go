package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/events"
	"github.com/otomo-live/otomo/services/session/internal/rules"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

// fakeParties returns whatever the test set, and records the arguments it was called with.
type fakeParties struct {
	party   *store.Party
	invite  *store.Invite
	notices []store.Notice
	err     error

	gotMaxSize  int
	gotTarget   string
	gotRevision int
	gotSettings map[string]string
	gotReady    *bool
	gotCheck    func(map[string]string) error
	launchCalls int
}

func (f *fakeParties) CreateParty(_ context.Context, _ string, maxSize int, settings map[string]string) (*store.Party, []store.Notice, error) {
	f.gotMaxSize, f.gotSettings = maxSize, settings
	return f.party, f.notices, f.err
}
func (f *fakeParties) UpdatePartySettings(_ context.Context, _ string, changes map[string]string, revision int) (*store.Party, []store.Notice, error) {
	f.gotSettings, f.gotRevision = changes, revision
	return f.party, f.notices, f.err
}
func (f *fakeParties) StartLaunch(_ context.Context, _ string, revision int, check func(map[string]string) error) (*store.Party, []store.Notice, []string, bool, error) {
	f.gotRevision, f.gotCheck = revision, check
	f.launchCalls++
	if f.err != nil {
		return nil, nil, nil, false, f.err
	}
	// The first press starts the launch; later presses find the party launching.
	started := f.launchCalls == 1
	var members []string
	if f.party != nil {
		for _, m := range f.party.Members {
			members = append(members, m.PlayerID)
		}
	}
	return f.party, f.notices, members, started, nil
}
func (f *fakeParties) SetReady(_ context.Context, _ string, ready bool) (*store.Party, []store.Notice, error) {
	f.gotReady = &ready
	return f.party, f.notices, f.err
}
func (f *fakeParties) GetParty(context.Context, string) (*store.Party, error) { return f.party, f.err }
func (f *fakeParties) InviteToParty(_ context.Context, _, target string, maxSize int) (*store.Invite, []store.Notice, error) {
	f.gotTarget, f.gotMaxSize = target, maxSize
	return f.invite, f.notices, f.err
}
func (f *fakeParties) AcceptInvite(_ context.Context, _, _ string, maxSize int) (*store.Party, []store.Notice, error) {
	f.gotMaxSize = maxSize
	return f.party, f.notices, f.err
}
func (f *fakeParties) DeclineInvite(context.Context, string, string) error { return f.err }
func (f *fakeParties) LeaveParty(context.Context, string) (*store.Party, []store.Notice, error) {
	return f.party, f.notices, f.err
}
func (f *fakeParties) KickFromParty(_ context.Context, _, target string, revision int) (*store.Party, []store.Notice, error) {
	f.gotTarget, f.gotRevision = target, revision
	return f.party, f.notices, f.err
}
func (f *fakeParties) PromoteInParty(_ context.Context, _, target string, revision int) (*store.Party, []store.Notice, error) {
	f.gotTarget, f.gotRevision = target, revision
	return f.party, f.notices, f.err
}

// recordingPublisher records every publish and the context it was given.
type recordingPublisher struct {
	mu        sync.Mutex
	published []store.Notice
	ctxErr    []error
}

func (p *recordingPublisher) Publish(ctx context.Context, player, typ string, payload any) (events.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pl, _ := payload.(map[string]any)
	p.published = append(p.published, store.Notice{PlayerID: player, Type: typ, Payload: pl})
	p.ctxErr = append(p.ctxErr, ctx.Err())
	return events.Event{}, nil
}

const otherPlayer = "018f4a3e-1c2d-7abc-8def-0123456789ac"

func sampleParty() *store.Party {
	return &store.Party{
		ID: "018f4a3e-0000-7000-8000-000000000001", LeaderID: testPlayer.String(), Revision: 3,
		Members: []store.Member{
			{PlayerID: testPlayer.String(), DisplayName: "Tanuki", Discriminator: 4417, JoinedAt: time.Unix(1, 0).UTC()},
			{PlayerID: otherPlayer, DisplayName: "Kitsune", Discriminator: 12, JoinedAt: time.Unix(2, 0).UTC()},
		},
	}
}

func partyCall(t *testing.T, h *Handlers, ctx context.Context, method, pattern, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(method+" "+PlayerPrefix+pattern, h.For(Route{Method: method, Path: PlayerPrefix + pattern}))
	req := httptest.NewRequest(method, PlayerPrefix+path, strings.NewReader(body))
	req = req.WithContext(auth.WithIdentity(ctx, &auth.Identity{Subject: testPlayer.String()}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestGetPartyShape(t *testing.T) {
	strict := rules.Defaults()
	strict.Party.MaxSize = 6
	h := &Handlers{Parties: &fakeParties{party: sampleParty()}, Rules: rules.Static{R: strict}}

	rec := partyCall(t, h, t.Context(), http.MethodGet, "/party", "/party", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /party = %d %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["revision"].(float64) != 3 || got["leader_id"] != testPlayer.String() || got["max_size"].(float64) != 6 {
		t.Errorf("body = %v; want revision 3, the leader, and max_size from the rules", got)
	}
	members := got["members"].([]any)
	if len(members) != 2 || members[0].(map[string]any)["display_name"] != "Tanuki" {
		t.Errorf("members = %v", members)
	}
}

// TestNothingIsPublishedWhenTheChangeFails: the handler publishes a change's notices
// only after the store call succeeded, which is after its transaction committed.
func TestNothingIsPublishedWhenTheChangeFails(t *testing.T) {
	pub := &recordingPublisher{}
	notices := []store.Notice{{PlayerID: otherPlayer, Type: events.TypePartyUpdated}}
	h := &Handlers{
		Parties:   &fakeParties{notices: notices, err: store.ErrPartyFull},
		Publisher: pub,
	}
	rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/invites/{invite_id}/accept",
		"/party/invites/018f4a3e-0000-7000-8000-000000000009/accept", "")
	if rec.Code != http.StatusConflict || errCode(t, rec) != "party_full" {
		t.Fatalf("accept = %d %s, want 409 party_full", rec.Code, rec.Body)
	}
	if len(pub.published) != 0 {
		t.Errorf("a failed change published %+v", pub.published)
	}
}

// TestNoticesArePublishedEvenIfTheClientLeft: the change committed, so the other members
// are told even when the caller's request context is already gone.
func TestNoticesArePublishedEvenIfTheClientLeft(t *testing.T) {
	pub := &recordingPublisher{}
	notices := []store.Notice{
		{PlayerID: testPlayer.String(), Type: events.TypePartyUpdated, Payload: map[string]any{"revision": 4}},
		{PlayerID: otherPlayer, Type: events.TypePartyUpdated, Payload: map[string]any{"revision": 4}},
	}
	h := &Handlers{Parties: &fakeParties{party: sampleParty(), notices: notices}, Publisher: pub}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	partyCall(t, h, ctx, http.MethodPost, "/party/invites/{invite_id}/accept",
		"/party/invites/018f4a3e-0000-7000-8000-000000000009/accept", "")

	if len(pub.published) != 2 {
		t.Fatalf("published %d events, want 2", len(pub.published))
	}
	for i, err := range pub.ctxErr {
		if err != nil {
			t.Errorf("publish %d ran on a dead context: %v", i, err)
		}
	}
}

func TestLeaderCallsRequireARevision(t *testing.T) {
	for _, route := range []string{"kick", "promote"} {
		fake := &fakeParties{party: sampleParty()}
		h := &Handlers{Parties: fake}
		pattern := "/party/" + route + "/{player_id}"
		path := "/party/" + route + "/" + otherPlayer

		if rec := partyCall(t, h, t.Context(), http.MethodPost, pattern, path, `{}`); rec.Code != http.StatusBadRequest || errCode(t, rec) != "revision_required" {
			t.Errorf("%s without a revision = %d %s, want 400 revision_required", route, rec.Code, rec.Body)
		}
		if rec := partyCall(t, h, t.Context(), http.MethodPost, pattern, "/party/"+route+"/not-a-uuid", `{"revision":3}`); rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_player_id" {
			t.Errorf("%s with a bad id = %d %s", route, rec.Code, rec.Body)
		}
		rec := partyCall(t, h, t.Context(), http.MethodPost, pattern, path, `{"revision":3}`)
		if rec.Code != http.StatusOK || fake.gotRevision != 3 || fake.gotTarget != otherPlayer {
			t.Errorf("%s = %d, store got revision %d target %s", route, rec.Code, fake.gotRevision, fake.gotTarget)
		}

		fake.err = &store.RevisionMismatchError{Current: 4}
		if rec := partyCall(t, h, t.Context(), http.MethodPost, pattern, path, `{"revision":3}`); rec.Code != http.StatusConflict || errCode(t, rec) != "revision_mismatch" {
			t.Errorf("%s with a stale revision = %d %s, want 409 revision_mismatch", route, rec.Code, rec.Body)
		}
	}
}

func TestPartyErrorMapping(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrNotInParty, http.StatusNotFound, "not_in_party"},
		{store.ErrAlreadyInParty, http.StatusConflict, "already_in_party"},
		{store.ErrNotLeader, http.StatusForbidden, "not_leader"},
		{store.ErrPartyFull, http.StatusConflict, "party_full"},
		{store.ErrInviteNotFound, http.StatusNotFound, "invite_not_found"},
		{store.ErrInviteExpired, http.StatusGone, "invite_expired"},
		{store.ErrNotAMember, http.StatusNotFound, "not_a_member"},
		{store.ErrAlreadyMember, http.StatusConflict, "already_member"},
		{store.ErrPlayerNotFound, http.StatusNotFound, "player_not_found"},
		{store.ErrBlocked, http.StatusForbidden, "blocked"},
		{store.ErrTargetIsYourself, http.StatusBadRequest, "invalid_target"},
		{errors.New("db down"), http.StatusInternalServerError, "internal_error"},
	} {
		h := &Handlers{Parties: &fakeParties{err: tt.err}}
		rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/invites", "/party/invites",
			`{"player_id":"`+otherPlayer+`"}`)
		if rec.Code != tt.status || errCode(t, rec) != tt.code {
			t.Errorf("%v = %d %s, want %d %s", tt.err, rec.Code, rec.Body, tt.status, tt.code)
		}
	}
}

func TestInviteAndCreateUseTheRulesPartySize(t *testing.T) {
	strict := rules.Defaults()
	strict.Party.MaxSize = 2
	fake := &fakeParties{party: sampleParty(), invite: &store.Invite{ID: "i", PartyID: "p", ToPlayer: otherPlayer}}
	h := &Handlers{Parties: fake, Rules: rules.Static{R: strict}}

	rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/invites", "/party/invites", `{"player_id":"`+otherPlayer+`"}`)
	if rec.Code != http.StatusCreated || fake.gotMaxSize != 2 || fake.gotTarget != otherPlayer {
		t.Errorf("invite = %d, store got max %d target %s", rec.Code, fake.gotMaxSize, fake.gotTarget)
	}
	if rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/invites", "/party/invites", `{"player_id":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("invite with a bad id = %d, want 400", rec.Code)
	}

	fake.gotMaxSize = 0
	if rec := partyCall(t, h, t.Context(), http.MethodPost, "/party", "/party", ""); rec.Code != http.StatusCreated || fake.gotMaxSize != 2 {
		t.Errorf("create = %d, store got max %d", rec.Code, fake.gotMaxSize)
	}
}

func TestDeclineAndLeaveAnswer204(t *testing.T) {
	h := &Handlers{Parties: &fakeParties{}}
	if rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/invites/{invite_id}/decline",
		"/party/invites/018f4a3e-0000-7000-8000-000000000009/decline", ""); rec.Code != http.StatusNoContent {
		t.Errorf("decline = %d, want 204", rec.Code)
	}
	if rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/leave", "/party/leave", ""); rec.Code != http.StatusNoContent {
		t.Errorf("leave = %d, want 204", rec.Code)
	}
}
