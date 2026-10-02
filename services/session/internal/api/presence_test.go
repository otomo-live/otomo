package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/session/internal/presence"
)

// fakePresence answers heartbeats with a fixed beat and statuses from a table.
type fakePresence struct {
	beat     presence.Beat
	err      error
	statuses map[string]string
	got      []string // statuses sent in heartbeats
}

func (f *fakePresence) Heartbeat(_ context.Context, _, status string) (presence.Beat, error) {
	f.got = append(f.got, status)
	return f.beat, f.err
}

func (f *fakePresence) Statuses(_ context.Context, players []string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]string{}
	for _, p := range players {
		out[p] = f.statuses[p]
		if out[p] == "" {
			out[p] = presence.StatusOffline
		}
	}
	return out, nil
}

func beat(t *testing.T, h *Handlers, body string) (int, string, string) {
	t.Helper()
	rec := partyCall(t, h, t.Context(), http.MethodPost, "/presence/heartbeat", "/presence/heartbeat", body)
	code := ""
	if rec.Code >= 400 {
		code = errCode(t, rec)
	}
	return rec.Code, code, rec.Header().Get("Retry-After")
}

func TestHeartbeat(t *testing.T) {
	p := &fakePresence{}
	h := &Handlers{Presence: p}

	for _, s := range []string{"online", "in_menus", "away"} {
		if code, _, _ := beat(t, h, `{"status":"`+s+`"}`); code != http.StatusNoContent {
			t.Errorf("heartbeat %s = %d, want 204", s, code)
		}
	}
	for _, body := range []string{`{"status":"offline"}`, `{"status":"busy"}`, `{}`} {
		if code, errc, _ := beat(t, h, body); code != http.StatusBadRequest || errc != "invalid_status" {
			t.Errorf("heartbeat %s = %d %s, want 400 invalid_status", body, code, errc)
		}
	}
	if code, errc, _ := beat(t, h, `{"status":"online","extra":1}`); code != http.StatusBadRequest || errc != "invalid_body" {
		t.Errorf("unknown field = %d %s, want 400 invalid_body", code, errc)
	}
	if len(p.got) != 3 {
		t.Errorf("the store saw %v, want only the three valid beats", p.got)
	}
}

// TestHeartbeatSpamIs429 is SES-B6: the 429 names how long to wait, rounded up.
func TestHeartbeatSpamIs429(t *testing.T) {
	h := &Handlers{Presence: &fakePresence{beat: presence.Beat{RetryAfter: 6200 * time.Millisecond}}}
	code, errc, retry := beat(t, h, `{"status":"online"}`)
	if code != http.StatusTooManyRequests || errc != "rate_limit_exceeded" || retry != "7" {
		t.Errorf("spam = %d %s Retry-After %q, want 429 rate_limit_exceeded 7", code, errc, retry)
	}
}

func TestHeartbeatFailures(t *testing.T) {
	if code, _, _ := beat(t, &Handlers{}, `{"status":"online"}`); code != http.StatusInternalServerError {
		t.Errorf("no presence configured = %d, want 500", code)
	}
	h := &Handlers{Presence: &fakePresence{err: errors.New("valkey down")}}
	if code, _, _ := beat(t, h, `{"status":"online"}`); code != http.StatusInternalServerError {
		t.Errorf("valkey down = %d, want 500", code)
	}
}

func partyMembers(t *testing.T, h *Handlers) []map[string]any {
	t.Helper()
	rec := partyCall(t, h, t.Context(), http.MethodGet, "/party", "/party", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /party = %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Members []map[string]any `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Members
}

// TestGetPartyShowsMemberPresence is doc 10 §5 row 13: each member's status.
func TestGetPartyShowsMemberPresence(t *testing.T) {
	h := &Handlers{
		Parties:  &fakeParties{party: sampleParty()},
		Presence: &fakePresence{statuses: map[string]string{testPlayer.String(): "away"}},
	}
	members := partyMembers(t, h)
	if members[0]["status"] != "away" || members[1]["status"] != "offline" {
		t.Errorf("statuses = %v, %v; want away and offline", members[0]["status"], members[1]["status"])
	}
}

// TestGetPartyWithoutPresenceStillAnswers: presence is a hint, and Valkey being down does
// not take GET /party with it.
func TestGetPartyWithoutPresenceStillAnswers(t *testing.T) {
	h := &Handlers{
		Parties:  &fakeParties{party: sampleParty()},
		Presence: &fakePresence{err: errors.New("valkey down")},
	}
	members := partyMembers(t, h)
	if _, has := members[0]["status"]; has || len(members) != 2 {
		t.Errorf("members = %v; want both, without a status", members)
	}
}
