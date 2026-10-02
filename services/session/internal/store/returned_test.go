package store

import (
	"testing"
	"time"

	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

// inGameLobby makes a party of leader and one member that has launched into a match on
// a new allocation.
func inGameLobby(t *testing.T, db *DB) (leader, member, alloc string, p *Party) {
	t.Helper()
	leader, member, ready := readyLobby(t, db)
	if _, _, _, started, err := db.StartLaunch(t.Context(), leader, ready.Revision, nil); err != nil || !started {
		t.Fatalf("StartLaunch = %v %v", started, err)
	}
	alloc = uuid.NewV7().String()
	p, _, done, err := db.FinishLaunch(t.Context(), ready.ID, alloc, "play.example.com", 27000)
	if err != nil || !done {
		t.Fatalf("FinishLaunch = %v %v", done, err)
	}
	return leader, member, alloc, p
}

// TestAfterAGameEveryMemberIsBackInTheSameLobby is LB-4's second criterion in the store:
// the same party, with the same members and settings, is forming again with every ready
// cleared and no match, and every member is told why.
func TestAfterAGameEveryMemberIsBackInTheSameLobby(t *testing.T) {
	db := testDB(t)
	leader, member, alloc, before := inGameLobby(t, db)

	got, notices, done, err := db.ReturnFromGame(t.Context(), before.ID, alloc, "ended")
	if err != nil || !done {
		t.Fatalf("ReturnFromGame = %v %v", done, err)
	}
	if got.ID != before.ID || got.State != StateForming || got.Revision != before.Revision+1 {
		t.Errorf("after the match = %+v; want the same party forming, one revision later", got)
	}
	if got.AllocationID != "" || got.MatchAddress != "" || got.MatchPort != 0 {
		t.Errorf("the match is still set: %q %q %d", got.AllocationID, got.MatchAddress, got.MatchPort)
	}
	if len(got.Members) != 2 || got.LeaderID != leader {
		t.Errorf("members = %+v, leader %s; want both, %s leading", got.Members, got.LeaderID, leader)
	}
	for _, m := range got.Members {
		if m.Ready {
			t.Errorf("%s is still ready", m.PlayerID)
		}
	}
	if got.Settings["difficulty"] != before.Settings["difficulty"] {
		t.Errorf("settings = %v, want them kept from %v", got.Settings, before.Settings)
	}

	returned := noticesOf(notices, events.TypePartyReturned)
	if !returned[leader] || !returned[member] {
		t.Errorf("party.returned went to %v, want both", returned)
	}
	for _, n := range notices {
		if n.Type == events.TypePartyReturned && (n.Payload["reason"] != "ended" || n.Payload["revision"] != got.Revision || n.Payload["party_id"] != got.ID) {
			t.Errorf("party.returned payload = %v", n.Payload)
		}
	}
	if up := noticesOf(notices, events.TypePartyUpdated); !up[leader] || !up[member] {
		t.Errorf("party.updated went to %v, want both", up)
	}

	// The lobby works again: ready up and launch.
	if _, _, err := db.SetReady(t.Context(), leader, true); err != nil {
		t.Fatal(err)
	}
	again, _, err := db.SetReady(t.Context(), member, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, started, err := db.StartLaunch(t.Context(), leader, again.Revision, nil); err != nil || !started {
		t.Errorf("relaunch = %v %v", started, err)
	}
}

// TestReturnFromGameIsIdempotent is doc 14 §4.4: a repeated callback, the repair poll
// racing the callback, a callback for an older allocation, one for a party that is not
// in a match, and one for a party that is gone all change nothing.
func TestReturnFromGameIsIdempotent(t *testing.T) {
	db := testDB(t)
	_, _, alloc, p := inGameLobby(t, db)

	if _, _, done, err := db.ReturnFromGame(t.Context(), p.ID, uuid.NewV7().String(), "ended"); err != nil || done {
		t.Errorf("another allocation's end = %v %v, want not done", done, err)
	}
	if _, _, done, err := db.ReturnFromGame(t.Context(), p.ID, alloc, "ended"); err != nil || !done {
		t.Fatalf("first return = %v %v", done, err)
	}
	after, err := db.GetParty(t.Context(), p.Members[0].PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, notices, done, err := db.ReturnFromGame(t.Context(), p.ID, alloc, "server_dead"); err != nil || done || len(notices) != 0 {
		t.Errorf("second return = %v %v %v, want not done and no notices", done, notices, err)
	}
	if again, _ := db.GetParty(t.Context(), p.Members[0].PlayerID); again.Revision != after.Revision {
		t.Errorf("revision moved from %d to %d on a repeated callback", after.Revision, again.Revision)
	}

	if _, _, done, err := db.ReturnFromGame(t.Context(), uuid.NewV7().String(), alloc, "ended"); err != nil || done {
		t.Errorf("a party that does not exist = %v %v, want not done", done, err)
	}
}

// TestADisbandedPartyIgnoresTheCallback is doc 14 §2 note 2: the last member leaves
// mid-match, and the callback later finds no party.
func TestADisbandedPartyIgnoresTheCallback(t *testing.T) {
	db := testDB(t)
	leader, member, alloc, p := inGameLobby(t, db)
	for _, who := range []string{member, leader} {
		if _, _, err := db.LeaveParty(t.Context(), who); err != nil {
			t.Fatalf("leave while in_game: %v", err)
		}
	}
	if _, _, done, err := db.ReturnFromGame(t.Context(), p.ID, alloc, "ended"); err != nil || done {
		t.Errorf("callback for a disbanded party = %v %v, want not done", done, err)
	}
}

func TestInGameLongerThanFindsOnlyOldMatches(t *testing.T) {
	db := testDB(t)
	_, _, alloc, p := inGameLobby(t, db)

	has := func(list []InGame) bool {
		for _, g := range list {
			if g.PartyID == p.ID {
				return g.AllocationID == alloc
			}
		}
		return false
	}
	fresh, err := db.InGameLongerThan(t.Context(), 30*time.Second)
	if err != nil || has(fresh) {
		t.Errorf("a match seconds old was listed: %v %v", fresh, err)
	}
	if _, err := db.Pool.Exec(t.Context(),
		`UPDATE party SET state_changed_at = now() - interval '40 seconds' WHERE party_id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	old, err := db.InGameLongerThan(t.Context(), 30*time.Second)
	if err != nil || !has(old) {
		t.Errorf("a match 40 s old was not listed: %v %v", old, err)
	}
	if _, _, done, err := db.ReturnFromGame(t.Context(), p.ID, alloc, "ended"); err != nil || !done {
		t.Fatal(done, err)
	}
	if back, err := db.InGameLongerThan(t.Context(), 0); err != nil || has(back) {
		t.Errorf("a returned party is still listed: %v %v", back, err)
	}
}
