package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/session/internal/events"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

const friendID = "018f4a3e-1c2d-7abc-8def-0123456789ad"

// fakeFriends returns canned answers and records what it was asked.
type fakeFriends struct {
	err       error
	list      []store.Friend
	found     string
	requested []string // "name#disc" looked up
	max       int
}

var oneNotice = []store.Notice{{PlayerID: friendID, Type: events.TypeFriendRequest}}

func (f *fakeFriends) FindPlayer(_ context.Context, name string, disc int) (string, error) {
	f.requested = append(f.requested, name)
	if f.found == "" {
		return "", store.ErrPlayerNotFound
	}
	return f.found, nil
}
func (f *fakeFriends) RequestFriend(_ context.Context, _, target string, max int) (store.Friend, []store.Notice, error) {
	f.max = max
	return store.Friend{PlayerID: target, DisplayName: "Kitsune", Discriminator: 12, State: store.FriendPending}, oneNotice, f.err
}
func (f *fakeFriends) AcceptFriend(_ context.Context, _, requester string, max int) (store.Friend, []store.Notice, error) {
	return store.Friend{PlayerID: requester, DisplayName: "Kitsune", Discriminator: 12}, oneNotice, f.err
}
func (f *fakeFriends) DeclineFriend(context.Context, string, string) ([]store.Notice, error) {
	return oneNotice, f.err
}
func (f *fakeFriends) RemoveFriend(context.Context, string, string) ([]store.Notice, error) {
	return oneNotice, f.err
}
func (f *fakeFriends) Block(context.Context, string, string) ([]store.Notice, error) {
	return oneNotice, f.err
}
func (f *fakeFriends) Unblock(context.Context, string, string) error { return f.err }
func (f *fakeFriends) ListFriends(context.Context, string) ([]store.Friend, error) {
	return f.list, f.err
}

type fakeLimiter struct {
	wait  time.Duration
	calls int
}

func (l *fakeLimiter) Allow(context.Context, string, string, int, time.Duration) (time.Duration, error) {
	l.calls++
	return l.wait, nil
}

// countingPresence counts Statuses calls, so a test can see one round trip per list.
type countingPresence struct {
	fakePresence
	calls int
}

func (c *countingPresence) Statuses(ctx context.Context, players []string) (map[string]string, error) {
	c.calls++
	return c.fakePresence.Statuses(ctx, players)
}

// TestFriendsListIsOneQueryAndOnePipeline is SES-C4: the list comes from one store call
// and every friend's presence from one Statuses call (one Valkey pipeline).
func TestFriendsListIsOneQueryAndOnePipeline(t *testing.T) {
	me := testPlayer.String()
	var list []store.Friend
	for i := range 200 {
		id := friendID[:len(friendID)-3] + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + "0"
		list = append(list, store.Friend{PlayerID: id, DisplayName: "F", Discriminator: i + 1, State: store.FriendAccepted})
	}
	list = append(list,
		store.Friend{PlayerID: otherPlayer, DisplayName: "In", Discriminator: 1, State: store.FriendPending, RequestedBy: otherPlayer},
		store.Friend{PlayerID: friendID, DisplayName: "Out", Discriminator: 2, State: store.FriendPending, RequestedBy: me},
	)
	pres := &countingPresence{fakePresence: fakePresence{statuses: map[string]string{list[0].PlayerID: "online"}}}
	h := &Handlers{Friends: &fakeFriends{list: list}, Presence: pres}

	rec := partyCall(t, h, t.Context(), http.MethodGet, "/friends", "/friends", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /friends = %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Friends, Incoming, Outgoing []map[string]any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Friends) != 200 || len(got.Incoming) != 1 || len(got.Outgoing) != 1 {
		t.Fatalf("friends %d, incoming %d, outgoing %d; want 200, 1, 1", len(got.Friends), len(got.Incoming), len(got.Outgoing))
	}
	if pres.calls != 1 {
		t.Errorf("%d presence reads for 200 friends, want 1", pres.calls)
	}
	if got.Friends[0]["status"] != "online" || got.Friends[1]["status"] != "offline" {
		t.Errorf("statuses = %v, %v", got.Friends[0]["status"], got.Friends[1]["status"])
	}
	if _, ok := got.Incoming[0]["status"]; ok {
		t.Error("a pending request carries a status")
	}
	// discriminator is a number (the decision for SE-5, as /me and the SDK).
	if _, ok := got.Incoming[0]["discriminator"].(float64); !ok {
		t.Errorf("discriminator = %#v, want a JSON number", got.Incoming[0]["discriminator"])
	}
}

func TestFriendRequest(t *testing.T) {
	ff := &fakeFriends{found: friendID}
	pub := &recordingPublisher{}
	lim := &fakeLimiter{}
	h := &Handlers{Friends: ff, Publisher: pub, Limits: lim}

	rec := partyCall(t, h, t.Context(), http.MethodPost, "/friends/requests", "/friends/requests",
		`{"display_name":" Kitsune ","discriminator":12}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("request = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["state"] != "pending" || body["player_id"] != friendID {
		t.Errorf("body = %v", body)
	}
	if ff.requested[0] != "Kitsune" || ff.max != 200 || len(pub.published) != 1 || lim.calls != 1 {
		t.Errorf("looked up %v with max %d, published %d, limited %d", ff.requested, ff.max, len(pub.published), lim.calls)
	}

	for _, bad := range []struct{ body, code string }{
		{`{"display_name":"Kitsune","discriminator":"12"}`, "invalid_body"},
		{`{"display_name":"Kitsune"}`, "invalid_player_tag"},
		{`{"display_name":"  ","discriminator":12}`, "invalid_player_tag"},
		{`{"display_name":"Kitsune","discriminator":10000}`, "invalid_player_tag"},
	} {
		rec := partyCall(t, h, t.Context(), http.MethodPost, "/friends/requests", "/friends/requests", bad.body)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != bad.code {
			t.Errorf("%s = %d %s, want 400 %s", bad.body, rec.Code, rec.Body, bad.code)
		}
	}
}

// TestFriendRequestSpamIs429 is SES-C5.
func TestFriendRequestSpamIs429(t *testing.T) {
	ff := &fakeFriends{found: friendID}
	h := &Handlers{Friends: ff, Limits: &fakeLimiter{wait: 90 * time.Second}}
	rec := partyCall(t, h, t.Context(), http.MethodPost, "/friends/requests", "/friends/requests",
		`{"display_name":"Kitsune","discriminator":12}`)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != "rate_limit_exceeded" || rec.Header().Get("Retry-After") != "90" {
		t.Errorf("spam = %d %s Retry-After %q", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}
	if len(ff.requested) != 0 {
		t.Error("a limited request reached the store")
	}
}

func TestFriendErrors(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrBlocked, http.StatusForbidden, "blocked"},
		{store.ErrAlreadyFriends, http.StatusConflict, "already_friends"},
		{store.ErrFriendLimit, http.StatusConflict, "friend_limit"},
		{store.ErrProfileNotFound, http.StatusNotFound, "profile_not_found"},
		{store.ErrPlayerNotFound, http.StatusNotFound, "player_not_found"},
		{store.ErrTargetIsYourself, http.StatusBadRequest, "invalid_target"},
		{errors.New("db down"), http.StatusInternalServerError, "internal_error"},
	} {
		h := &Handlers{Friends: &fakeFriends{found: friendID, err: c.err}}
		rec := partyCall(t, h, t.Context(), http.MethodPost, "/friends/requests", "/friends/requests",
			`{"display_name":"Kitsune","discriminator":12}`)
		if rec.Code != c.status || errCode(t, rec) != c.code {
			t.Errorf("%v = %d %s, want %d %s", c.err, rec.Code, rec.Body, c.status, c.code)
		}
	}
	if rec := partyCall(t, &Handlers{Friends: &fakeFriends{}}, t.Context(), http.MethodPost, "/friends/requests", "/friends/requests",
		`{"display_name":"Nobody","discriminator":1}`); rec.Code != http.StatusNotFound || errCode(t, rec) != "player_not_found" {
		t.Errorf("unknown player = %d %s", rec.Code, rec.Body)
	}
}

func TestFriendAnswersAndBlocks(t *testing.T) {
	pub := &recordingPublisher{}
	h := &Handlers{Friends: &fakeFriends{}, Publisher: pub}
	for _, c := range []struct {
		method, pattern, path string
		want                  int
	}{
		{http.MethodPost, "/friends/requests/{player_id}/accept", "/friends/requests/" + friendID + "/accept", http.StatusOK},
		{http.MethodPost, "/friends/requests/{player_id}/decline", "/friends/requests/" + friendID + "/decline", http.StatusNoContent},
		{http.MethodDelete, "/friends/{player_id}", "/friends/" + friendID, http.StatusNoContent},
		{http.MethodPost, "/blocks/{player_id}", "/blocks/" + friendID, http.StatusNoContent},
		{http.MethodDelete, "/blocks/{player_id}", "/blocks/" + friendID, http.StatusNoContent},
		{http.MethodPost, "/blocks/{player_id}", "/blocks/not-a-uuid", http.StatusBadRequest},
	} {
		if rec := partyCall(t, h, t.Context(), c.method, c.pattern, c.path, ""); rec.Code != c.want {
			t.Errorf("%s %s = %d %s, want %d", c.method, c.path, rec.Code, rec.Body, c.want)
		}
	}
	if len(pub.published) != 4 {
		t.Errorf("%d events published, want one each for accept, decline, remove and block", len(pub.published))
	}

	for _, c := range []struct {
		err    error
		method string
		pat    string
		path   string
		code   string
	}{
		{store.ErrRequestNotFound, http.MethodPost, "/friends/requests/{player_id}/accept", "/friends/requests/" + friendID + "/accept", "request_not_found"},
		{store.ErrNotFriends, http.MethodDelete, "/friends/{player_id}", "/friends/" + friendID, "not_friends"},
	} {
		h := &Handlers{Friends: &fakeFriends{err: c.err}}
		if rec := partyCall(t, h, t.Context(), c.method, c.pat, c.path, ""); rec.Code != http.StatusNotFound || errCode(t, rec) != c.code {
			t.Errorf("%v = %d %s, want 404 %s", c.err, rec.Code, rec.Body, c.code)
		}
	}
}
