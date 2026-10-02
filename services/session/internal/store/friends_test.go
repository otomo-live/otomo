package store

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

// befriend stores a friendship between a and b in state, lower id first.
func befriend(t *testing.T, db *DB, a, b, state string) {
	t.Helper()
	lo, hi := pair(a, b)
	if _, err := db.Pool.Exec(t.Context(),
		`INSERT INTO friendship (player_lo, player_hi, state, requested_by) VALUES ($1, $2, $3, $4)`,
		lo, hi, state, a); err != nil {
		t.Fatalf("insert friendship: %v", err)
	}
}

func stateOf(t *testing.T, db *DB, a, b string) string {
	t.Helper()
	lo, hi := pair(a, b)
	var s string
	err := db.Pool.QueryRow(t.Context(),
		`SELECT coalesce((SELECT state FROM friendship WHERE player_lo = $1 AND player_hi = $2), '')`, lo, hi).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAcceptedFriendsSearchesBothSides: a friendship is one row, and each of its two
// players sees the other; a pending request is not a friend yet.
func TestAcceptedFriendsSearchesBothSides(t *testing.T) {
	db := testDB(t)
	me, a, b, pending := player(t, db), player(t, db), player(t, db), player(t, db)
	befriend(t, db, me, a, FriendAccepted)
	befriend(t, db, me, b, FriendAccepted)
	befriend(t, db, me, pending, FriendPending)

	got, err := db.AcceptedFriends(t.Context(), me)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{a, b}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("friends of me = %v, want %v", got, want)
	}
	if got, _ := db.AcceptedFriends(t.Context(), a); !slices.Equal(got, []string{me}) {
		t.Errorf("friends of a = %v, want [me]", got)
	}
}

func TestFindPlayerIgnoresCase(t *testing.T) {
	db := testDB(t)
	p := player(t, db)
	var name string
	if err := db.Pool.QueryRow(t.Context(), `SELECT display_name FROM player_profile WHERE player_id = $1`, p).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if got, err := db.FindPlayer(t.Context(), strings.ToLower(name), 1); err != nil || got != p {
		t.Errorf("FindPlayer(%s#1) = %s, %v", strings.ToLower(name), got, err)
	}
	if _, err := db.FindPlayer(t.Context(), name, 2); !errors.Is(err, ErrPlayerNotFound) {
		t.Errorf("wrong discriminator = %v, want ErrPlayerNotFound", err)
	}
}

// TestRequestAcceptDeclineRemove is SES-C1 and SES-C2: each step tells the other player.
func TestRequestAcceptDeclineRemove(t *testing.T) {
	db := testDB(t)
	a, b := player(t, db), player(t, db)

	them, notices, err := db.RequestFriend(t.Context(), a, b, 200)
	if err != nil || them.State != FriendPending || them.PlayerID != b {
		t.Fatalf("request = %+v, %v", them, err)
	}
	if len(notices) != 1 || notices[0].PlayerID != b || notices[0].Type != events.TypeFriendRequest {
		t.Errorf("request notices = %+v", notices)
	}
	// Asking again changes nothing and tells nobody.
	if again, n, err := db.RequestFriend(t.Context(), a, b, 200); err != nil || again.State != FriendPending || len(n) != 0 {
		t.Errorf("second request = %+v %v %v", again, n, err)
	}
	// Only b can accept a's request.
	if _, _, err := db.AcceptFriend(t.Context(), a, b, 200); !errors.Is(err, ErrRequestNotFound) {
		t.Errorf("accepting your own request = %v, want ErrRequestNotFound", err)
	}
	if _, notices, err := db.AcceptFriend(t.Context(), b, a, 200); err != nil || notices[0].PlayerID != a || notices[0].Type != events.TypeFriendAccepted {
		t.Fatalf("accept = %+v, %v", notices, err)
	}
	if stateOf(t, db, a, b) != FriendAccepted {
		t.Error("not friends after the accept")
	}
	if _, _, err := db.RequestFriend(t.Context(), b, a, 200); !errors.Is(err, ErrAlreadyFriends) {
		t.Errorf("request between friends = %v, want ErrAlreadyFriends", err)
	}
	if notices, err := db.RemoveFriend(t.Context(), a, b); err != nil || notices[0].PlayerID != b || notices[0].Type != events.TypeFriendRemoved {
		t.Errorf("remove = %+v, %v", notices, err)
	}
	if _, err := db.RemoveFriend(t.Context(), a, b); !errors.Is(err, ErrNotFriends) {
		t.Errorf("second remove = %v, want ErrNotFriends", err)
	}

	if _, _, err := db.RequestFriend(t.Context(), a, b, 200); err != nil {
		t.Fatal(err)
	}
	if notices, err := db.DeclineFriend(t.Context(), b, a); err != nil || notices[0].PlayerID != a {
		t.Errorf("decline = %+v, %v", notices, err)
	}
	if stateOf(t, db, a, b) != "" {
		t.Error("the request survived a decline")
	}
}

// TestMutualSimultaneousRequestsEndAsOneFriendship is SES-C1's criterion.
func TestMutualSimultaneousRequestsEndAsOneFriendship(t *testing.T) {
	db := testDB(t)
	for range 5 {
		a, b := player(t, db), player(t, db)
		var wg sync.WaitGroup
		start := make(chan struct{})
		states := make([]string, 2)
		errs := make([]error, 2)
		for i, ask := range [][2]string{{a, b}, {b, a}} {
			wg.Go(func() {
				<-start
				f, _, err := db.RequestFriend(t.Context(), ask[0], ask[1], 200)
				states[i], errs[i] = f.State, err
			})
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if stateOf(t, db, a, b) != FriendAccepted {
			t.Errorf("after two crossed requests the friendship is %q, want accepted", stateOf(t, db, a, b))
		}
		slices.Sort(states)
		if !slices.Equal(states, []string{FriendAccepted, FriendPending}) {
			t.Errorf("answers = %v, want one pending and one accepted", states)
		}
	}
}

func TestTheFriendLimit(t *testing.T) {
	db := testDB(t)
	a, b, c := player(t, db), player(t, db), player(t, db)
	befriend(t, db, a, b, FriendAccepted)
	if _, _, err := db.RequestFriend(t.Context(), a, c, 1); !errors.Is(err, ErrFriendLimit) {
		t.Errorf("request past the limit = %v, want ErrFriendLimit", err)
	}
	// c asks a: pending is fine, but a cannot accept past the limit.
	if _, _, err := db.RequestFriend(t.Context(), c, a, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AcceptFriend(t.Context(), a, c, 1); !errors.Is(err, ErrFriendLimit) {
		t.Errorf("accept past the limit = %v, want ErrFriendLimit", err)
	}
}

// TestABlockedPlayerCannotRequestOrInvite is SE-5's criterion, and SES-C3: the block,
// the end of the friendship and of every party invite between the two are one
// transaction.
func TestABlockedPlayerCannotRequestOrInvite(t *testing.T) {
	db := testDB(t)
	a, b := player(t, db), player(t, db)
	befriend(t, db, a, b, FriendAccepted)
	if _, _, err := db.CreateParty(t.Context(), a, 4, seedSettings); err != nil {
		t.Fatal(err)
	}
	inv, _, err := db.InviteToParty(t.Context(), a, b, 4)
	if err != nil {
		t.Fatal(err)
	}

	notices, err := db.Block(t.Context(), b, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].PlayerID != a || notices[0].Type != events.TypeFriendRemoved {
		t.Errorf("block notices = %+v, want friend.removed to a", notices)
	}
	if stateOf(t, db, a, b) != "" {
		t.Error("the friendship survived the block")
	}
	var invites int
	if err := db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM party_invite WHERE invite_id = $1`, inv.ID).Scan(&invites); err != nil || invites != 0 {
		t.Errorf("%d invites left after the block, %v", invites, err)
	}
	if _, _, err := db.AcceptInvite(t.Context(), b, inv.ID, 4); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("accepting the old invite = %v, want ErrInviteNotFound", err)
	}

	// Neither side can ask or invite, whoever blocked.
	for _, from := range [][2]string{{a, b}, {b, a}} {
		if _, _, err := db.RequestFriend(t.Context(), from[0], from[1], 200); !errors.Is(err, ErrBlocked) {
			t.Errorf("request %s to %s = %v, want ErrBlocked", from[0][:8], from[1][:8], err)
		}
	}
	if _, _, err := db.InviteToParty(t.Context(), a, b, 4); !errors.Is(err, ErrBlocked) {
		t.Errorf("invite after the block = %v, want ErrBlocked", err)
	}

	// Blocking twice changes nothing; unblocking lets them ask again but does not
	// restore the friendship.
	if n, err := db.Block(t.Context(), b, a); err != nil || len(n) != 0 {
		t.Errorf("second block = %v %v", n, err)
	}
	if err := db.Unblock(t.Context(), b, a); err != nil {
		t.Fatal(err)
	}
	if err := db.Unblock(t.Context(), b, a); err != nil {
		t.Errorf("second unblock = %v", err)
	}
	if stateOf(t, db, a, b) != "" {
		t.Error("unblocking restored the friendship")
	}
	if _, _, err := db.RequestFriend(t.Context(), a, b, 200); err != nil {
		t.Errorf("request after the unblock = %v", err)
	}
}

// TestAnInviteThatRacedTheBlockCannotBeAccepted: AcceptInvite checks the block itself.
func TestAnInviteThatRacedTheBlockCannotBeAccepted(t *testing.T) {
	db := testDB(t)
	a, b := player(t, db), player(t, db)
	if _, _, err := db.CreateParty(t.Context(), a, 4, seedSettings); err != nil {
		t.Fatal(err)
	}
	inv, _, err := db.InviteToParty(t.Context(), a, b, 4)
	if err != nil {
		t.Fatal(err)
	}
	// The block row without the invite cleanup, as if the invite committed after it.
	if _, err := db.Pool.Exec(t.Context(), `INSERT INTO block (blocker, blocked) VALUES ($1, $2)`, b, a); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AcceptInvite(t.Context(), b, inv.ID, 4); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("accept across a block = %v, want ErrInviteNotFound", err)
	}
}

func TestListFriendsSortsByDirection(t *testing.T) {
	db := testDB(t)
	me, friend, in, out := player(t, db), player(t, db), player(t, db), player(t, db)
	befriend(t, db, me, friend, FriendAccepted)
	befriend(t, db, in, me, FriendPending)
	befriend(t, db, me, out, FriendPending)

	list, err := db.ListFriends(t.Context(), me)
	if err != nil || len(list) != 3 {
		t.Fatalf("ListFriends = %+v, %v", list, err)
	}
	by := map[string]Friend{}
	for _, f := range list {
		by[f.PlayerID] = f
		if f.DisplayName == "" || f.Discriminator != 1 {
			t.Errorf("%s has no public name: %+v", f.PlayerID, f)
		}
	}
	if by[friend].State != FriendAccepted || by[in].RequestedBy != in || by[out].RequestedBy != me {
		t.Errorf("list = %+v", by)
	}
}
