package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/session/internal/allocator"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

type recordingLauncher struct {
	mu     sync.Mutex
	starts []string
}

func (l *recordingLauncher) Start(partyID string, _ []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.starts = append(l.starts, partyID)
}

type fakeTickets struct {
	err     error
	gotID   string
	gotUser string
}

func (f *fakeTickets) Ticket(_ context.Context, allocationID, playerID string) (string, time.Time, error) {
	f.gotID, f.gotUser = allocationID, playerID
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return "eyJ.ticket.sig", time.Unix(1_800_000_000, 0).UTC(), nil
}

func launchCall(t *testing.T, h *Handlers, body string) (int, string) {
	t.Helper()
	rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/launch", "/party/launch", body)
	code := ""
	if rec.Code >= 400 {
		code = errCode(t, rec)
	}
	return rec.Code, code
}

// TestTwoLaunchPressesStartOneAllocation is LB-3's first criterion at the handler: the
// second press finds the party launching, answers 202 and starts nothing.
func TestTwoLaunchPressesStartOneAllocation(t *testing.T) {
	launcher := &recordingLauncher{}
	h := &Handlers{Parties: &fakeParties{party: sampleParty()}, Launcher: launcher}

	for press := 1; press <= 2; press++ {
		if status, code := launchCall(t, h, `{"revision":3}`); status != http.StatusAccepted {
			t.Fatalf("press %d = %d %s, want 202", press, status, code)
		}
	}
	if len(launcher.starts) != 1 {
		t.Errorf("two presses started %d launches, want 1", len(launcher.starts))
	}
}

func TestLaunchRefusals(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		body   string
		status int
		code   string
	}{
		{"no revision", nil, `{}`, 400, "revision_required"},
		{"not ready", store.ErrNotReady, `{"revision":3}`, 409, "not_ready"},
		{"stale revision", &store.RevisionMismatchError{Current: 4}, `{"revision":3}`, 409, "revision_mismatch"},
		{"in game", store.ErrPartyLocked, `{"revision":3}`, 409, "party_locked"},
		{"not leader", store.ErrNotLeader, `{"revision":3}`, 403, "not_leader"},
		{"stale settings", &store.InvalidSettingsError{Reason: "x"}, `{"revision":3}`, 409, "invalid_settings"},
	} {
		launcher := &recordingLauncher{}
		h := &Handlers{Parties: &fakeParties{party: sampleParty(), err: tt.err}, Launcher: launcher}
		if status, code := launchCall(t, h, tt.body); status != tt.status || code != tt.code {
			t.Errorf("%s = %d %s, want %d %s", tt.name, status, code, tt.status, tt.code)
		}
		if len(launcher.starts) != 0 {
			t.Errorf("%s started a launch", tt.name)
		}
	}

	h := &Handlers{Parties: &fakeParties{party: sampleParty()}}
	if status, code := launchCall(t, h, `{"revision":3}`); status != http.StatusServiceUnavailable || code != "launch_unavailable" {
		t.Errorf("no launcher = %d %s, want 503 launch_unavailable", status, code)
	}
}

// TestLaunchChecksTheSettingsAgainstTheRules: settings saved before the rules changed
// are checked at launch.
func TestLaunchChecksTheSettingsAgainstTheRules(t *testing.T) {
	fake := &fakeParties{party: sampleParty()}
	h := &Handlers{Parties: fake, Launcher: &recordingLauncher{}}
	launchCall(t, h, `{"revision":3}`)
	if fake.gotCheck == nil {
		t.Fatal("no settings check was passed to the store")
	}
	if err := fake.gotCheck(map[string]string{"difficulty": "hard"}); err != nil {
		t.Errorf("allowed settings refused: %v", err)
	}
	if err := fake.gotCheck(map[string]string{"difficulty": "nightmare"}); err == nil {
		t.Error("a value the rules do not allow passed the launch check")
	}
}

func inGameParty() *store.Party {
	p := sampleParty()
	p.State = store.StateInGame
	p.AllocationID = "0b7e0000-0000-7000-8000-000000000001"
	p.MatchAddress, p.MatchPort = "play.example.com", 27000
	return p
}

func TestLaunchTicket(t *testing.T) {
	tickets := &fakeTickets{}
	h := &Handlers{Parties: &fakeParties{party: inGameParty()}, Tickets: tickets}

	rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/launch/ticket", "/party/launch/ticket", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ticket = %d %s", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["address"] != "play.example.com" || got["port"].(float64) != 27000 || got["ticket"] != "eyJ.ticket.sig" || got["ticket_expires_at"] == nil {
		t.Errorf("ticket body = %s", rec.Body)
	}
	if tickets.gotID != inGameParty().AllocationID || tickets.gotUser != testPlayer.String() {
		t.Errorf("asked the Allocator for %s / %s", tickets.gotID, tickets.gotUser)
	}

	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{allocator.ErrAllocationEnded, 409, "match_ended"},
		{allocator.ErrNotFound, 404, "not_in_match"},
		{errors.New("timeout"), 503, "allocator_unavailable"},
	} {
		h := &Handlers{Parties: &fakeParties{party: inGameParty()}, Tickets: &fakeTickets{err: tt.err}}
		rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/launch/ticket", "/party/launch/ticket", "")
		if rec.Code != tt.status || errCode(t, rec) != tt.code {
			t.Errorf("%v = %d %s, want %d %s", tt.err, rec.Code, rec.Body, tt.status, tt.code)
		}
	}

	forming := sampleParty()
	forming.State = store.StateForming
	h = &Handlers{Parties: &fakeParties{party: forming}, Tickets: &fakeTickets{}}
	if rec := partyCall(t, h, t.Context(), http.MethodPost, "/party/launch/ticket", "/party/launch/ticket", ""); rec.Code != http.StatusConflict || errCode(t, rec) != "not_in_game" {
		t.Errorf("ticket while forming = %d %s, want 409 not_in_game", rec.Code, rec.Body)
	}
}

// TestGetPartyShowsTheMatchWithoutATicket: doc 14 §2.1.
func TestGetPartyShowsTheMatchWithoutATicket(t *testing.T) {
	h := &Handlers{Parties: &fakeParties{party: inGameParty()}}
	rec := partyCall(t, h, t.Context(), http.MethodGet, "/party", "/party", "")
	var got struct {
		Match map[string]any `json:"match"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Match["address"] != "play.example.com" || got.Match["allocation_id"] == nil || got.Match["ticket"] != nil {
		t.Errorf("match = %v", got.Match)
	}

	h = &Handlers{Parties: &fakeParties{party: sampleParty()}}
	rec = partyCall(t, h, t.Context(), http.MethodGet, "/party", "/party", "")
	var forming map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &forming); err != nil {
		t.Fatal(err)
	}
	if _, ok := forming["match"]; ok {
		t.Errorf("a party that is not in_game shows a match: %s", rec.Body)
	}
}
