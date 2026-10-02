package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

var profileSeq atomic.Int64

// player makes a player with a profile and removes everything the test gave them: their
// party (if they led or were in one), their invites and their profile. A reused test
// database therefore never carries a party from an earlier run.
func player(t *testing.T, db *DB) string {
	t.Helper()
	id := uuid.NewV7().String()
	name := fmt.Sprintf("P%d_%d", time.Now().UnixNano()%1e9, profileSeq.Add(1))
	if _, err := db.Pool.Exec(t.Context(),
		`INSERT INTO player_profile (player_id, display_name, discriminator) VALUES ($1, $2, 1)`,
		id, name); err != nil {
		t.Fatalf("insert profile: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.Pool.Exec(ctx, `DELETE FROM party WHERE leader_id = $1
			OR party_id IN (SELECT party_id FROM party_member WHERE player_id = $1)`, id)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM party_invite WHERE to_player = $1 OR from_player = $1`, id)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM block WHERE blocker = $1 OR blocked = $1`, id)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM friendship WHERE player_lo = $1 OR player_hi = $1`, id)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM player_profile WHERE player_id = $1`, id)
	})
	return id
}

// join invites target into leader's party and accepts it.
func join(t *testing.T, db *DB, leader, target string) *Party {
	t.Helper()
	inv, _, err := db.InviteToParty(t.Context(), leader, target, 4)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	p, _, err := db.AcceptInvite(t.Context(), target, inv.ID, 4)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return p
}

func noticesOf(ns []Notice, typ string) map[string]bool {
	out := map[string]bool{}
	for _, n := range ns {
		if n.Type == typ {
			out[n.PlayerID] = true
		}
	}
	return out
}

func TestCreatePartyMakesTheCallerLeader(t *testing.T) {
	db := testDB(t)
	a := player(t, db)

	p, notices, err := db.CreateParty(t.Context(), a, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.LeaderID != a || len(p.Members) != 1 || p.Members[0].PlayerID != a || p.Revision != 1 {
		t.Errorf("party = %+v", p)
	}
	if !noticesOf(notices, events.TypePartyUpdated)[a] {
		t.Errorf("notices = %+v, want party.updated to the creator", notices)
	}
	got, err := db.GetParty(t.Context(), a)
	if err != nil || got.ID != p.ID {
		t.Errorf("GetParty = %+v, %v", got, err)
	}
}

// TestNobodyCanBeInTwoParties is an SE-6 acceptance criterion, including the race where
// one player accepts invites to two parties at once.
func TestNobodyCanBeInTwoParties(t *testing.T) {
	db := testDB(t)
	a, b, c := player(t, db), player(t, db), player(t, db)

	if _, _, err := db.CreateParty(t.Context(), a, 4, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateParty(t.Context(), a, 4, nil); !errors.Is(err, ErrAlreadyInParty) {
		t.Errorf("second create = %v, want ErrAlreadyInParty", err)
	}
	if _, _, err := db.CreateParty(t.Context(), b, 4, nil); err != nil {
		t.Fatal(err)
	}

	// b is in their own party, so accepting a's invite must fail.
	inv, _, err := db.InviteToParty(t.Context(), a, b, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AcceptInvite(t.Context(), b, inv.ID, 4); !errors.Is(err, ErrAlreadyInParty) {
		t.Errorf("accept while in a party = %v, want ErrAlreadyInParty", err)
	}

	// c accepts invites from both parties at the same moment: exactly one wins.
	invA, _, err := db.InviteToParty(t.Context(), a, c, 4)
	if err != nil {
		t.Fatal(err)
	}
	invB, _, err := db.InviteToParty(t.Context(), b, c, 4)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i, id := range []string{invA.ID, invB.ID} {
		wg.Go(func() {
			<-start
			_, _, errs[i] = db.AcceptInvite(t.Context(), c, id, 4)
		})
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrAlreadyInParty):
		default:
			t.Errorf("concurrent accept: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d concurrent accepts succeeded, want exactly 1", ok)
	}
	var memberships int
	if err := db.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM party_member WHERE player_id = $1`, c).Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if memberships != 1 {
		t.Errorf("c is in %d parties, want 1", memberships)
	}
}

// TestConcurrentAcceptsNeverOverfill is SES-D3: five invitees racing for the three free
// places in a party of four.
func TestConcurrentAcceptsNeverOverfill(t *testing.T) {
	db := testDB(t)
	leader := player(t, db)
	if _, _, err := db.CreateParty(t.Context(), leader, 4, nil); err != nil {
		t.Fatal(err)
	}

	var invites []string
	var invitees []string
	for range 5 {
		p := player(t, db)
		inv, _, err := db.InviteToParty(t.Context(), leader, p, 4)
		if err != nil {
			t.Fatal(err)
		}
		invites, invitees = append(invites, inv.ID), append(invitees, p)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, len(invites))
	for i := range invites {
		wg.Go(func() {
			<-start
			_, _, errs[i] = db.AcceptInvite(t.Context(), invitees[i], invites[i], 4)
		})
	}
	close(start)
	wg.Wait()

	joined, full := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			joined++
		case errors.Is(err, ErrPartyFull):
			full++
		default:
			t.Errorf("accept: %v", err)
		}
	}
	if joined != 3 || full != 2 {
		t.Errorf("joined %d, refused as full %d; want 3 and 2", joined, full)
	}
	p, err := db.GetParty(t.Context(), leader)
	if err != nil || len(p.Members) != 4 {
		t.Errorf("party has %d members (%v), want 4", len(p.Members), err)
	}
}

// TestARolledBackJoinNotifiesNobody is an SE-6 acceptance criterion: a join whose
// transaction fails after its writes leaves no member, keeps the invite, and returns no
// notices for the caller to publish.
func TestARolledBackJoinNotifiesNobody(t *testing.T) {
	db := testDB(t)
	leader, joiner := player(t, db), player(t, db)
	before, _, err := db.CreateParty(t.Context(), leader, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := db.InviteToParty(t.Context(), leader, joiner, 4)
	if err != nil {
		t.Fatal(err)
	}

	boom := errors.New("fail before commit")
	db.beforeCommit = func(context.Context, pgx.Tx) error { return boom }
	p, notices, err := db.AcceptInvite(t.Context(), joiner, inv.ID, 4)
	db.beforeCommit = nil

	if !errors.Is(err, boom) || p != nil || len(notices) != 0 {
		t.Fatalf("AcceptInvite = %+v, %+v, %v; want the error and no notices", p, notices, err)
	}
	after, err := db.GetParty(t.Context(), leader)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Members) != 1 || after.Revision != before.Revision {
		t.Errorf("after the rollback the party is %+v, want it unchanged", after)
	}
	if _, err := db.GetParty(t.Context(), joiner); !errors.Is(err, ErrNotInParty) {
		t.Errorf("the joiner's party after the rollback = %v, want ErrNotInParty", err)
	}
	// The invite survived, so the joiner can try again.
	if _, _, err := db.AcceptInvite(t.Context(), joiner, inv.ID, 4); err != nil {
		t.Errorf("accept after the rollback: %v", err)
	}
}

// TestLeaderLeavingPromotesTheLongestStanding is an SE-6 acceptance criterion.
func TestLeaderLeavingPromotesTheLongestStanding(t *testing.T) {
	db := testDB(t)
	leader, first, second := player(t, db), player(t, db), player(t, db)
	if _, _, err := db.CreateParty(t.Context(), leader, 4, nil); err != nil {
		t.Fatal(err)
	}
	join(t, db, leader, first)
	join(t, db, leader, second)

	p, notices, err := db.LeaveParty(t.Context(), leader)
	if err != nil {
		t.Fatal(err)
	}
	if p.LeaderID != first {
		t.Errorf("leader after the leader left = %s, want the longest-standing member %s", p.LeaderID, first)
	}
	got := noticesOf(notices, events.TypePartyUpdated)
	if !got[first] || !got[second] || got[leader] {
		t.Errorf("party.updated went to %v, want the two who stayed", got)
	}
}

func TestLastMemberLeavingDisbands(t *testing.T) {
	db := testDB(t)
	a := player(t, db)
	created, _, err := db.CreateParty(t.Context(), a, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, notices, err := db.LeaveParty(t.Context(), a)
	if err != nil || p != nil || len(notices) != 0 {
		t.Fatalf("LeaveParty = %+v, %+v, %v; want a disbanded party and no notices", p, notices, err)
	}
	var exists bool
	if err := db.Pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM party WHERE party_id = $1)`, created.ID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("the empty party still exists")
	}
	if _, _, err := db.LeaveParty(t.Context(), a); !errors.Is(err, ErrNotInParty) {
		t.Errorf("leaving again = %v, want ErrNotInParty", err)
	}
}

// TestLeaderCallsNeedTheCurrentRevision is an SE-6 acceptance criterion: revision is
// required on every leader call, and a stale one is refused.
func TestLeaderCallsNeedTheCurrentRevision(t *testing.T) {
	db := testDB(t)
	leader, m1, m2 := player(t, db), player(t, db), player(t, db)
	if _, _, err := db.CreateParty(t.Context(), leader, 4, nil); err != nil {
		t.Fatal(err)
	}
	join(t, db, leader, m1)
	p := join(t, db, leader, m2)

	var stale *RevisionMismatchError
	if _, _, err := db.KickFromParty(t.Context(), leader, m1, p.Revision-1); !errors.As(err, &stale) || stale.Current != p.Revision {
		t.Errorf("kick with a stale revision = %v, want RevisionMismatchError{%d}", err, p.Revision)
	}
	if _, _, err := db.PromoteInParty(t.Context(), leader, m1, p.Revision+1); !errors.As(err, &stale) {
		t.Errorf("promote with a future revision = %v, want RevisionMismatchError", err)
	}
	if _, _, err := db.KickFromParty(t.Context(), m1, m2, p.Revision); !errors.Is(err, ErrNotLeader) {
		t.Errorf("kick by a member = %v, want ErrNotLeader", err)
	}

	kicked, notices, err := db.KickFromParty(t.Context(), leader, m1, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if kicked.Revision != p.Revision+1 || kicked.has(m1) {
		t.Errorf("after the kick the party is %+v", kicked)
	}
	if !noticesOf(notices, events.TypePartyKicked)[m1] || !noticesOf(notices, events.TypePartyUpdated)[m2] {
		t.Errorf("notices = %+v, want party.kicked to m1 and party.updated to m2", notices)
	}

	promoted, _, err := db.PromoteInParty(t.Context(), leader, m2, kicked.Revision)
	if err != nil || promoted.LeaderID != m2 {
		t.Errorf("promote = %+v, %v", promoted, err)
	}
	if _, _, err := db.KickFromParty(t.Context(), leader, m2, promoted.Revision); !errors.Is(err, ErrNotLeader) {
		t.Errorf("the old leader kicking = %v, want ErrNotLeader", err)
	}
	if _, _, err := db.KickFromParty(t.Context(), m2, m1, promoted.Revision); !errors.Is(err, ErrNotAMember) {
		t.Errorf("kicking someone not in the party = %v, want ErrNotAMember", err)
	}
}

func TestInviteRules(t *testing.T) {
	db := testDB(t)
	a, b, blocker := player(t, db), player(t, db), player(t, db)
	if _, _, err := db.CreateParty(t.Context(), a, 4, nil); err != nil {
		t.Fatal(err)
	}

	if _, _, err := db.InviteToParty(t.Context(), a, a, 4); !errors.Is(err, ErrTargetIsYourself) {
		t.Errorf("invite yourself = %v", err)
	}
	if _, _, err := db.InviteToParty(t.Context(), a, uuid.NewV7().String(), 4); !errors.Is(err, ErrPlayerNotFound) {
		t.Errorf("invite a player with no profile = %v", err)
	}
	if _, _, err := db.InviteToParty(t.Context(), b, a, 4); !errors.Is(err, ErrNotInParty) {
		t.Errorf("invite while in no party = %v", err)
	}

	if _, err := db.Pool.Exec(t.Context(),
		`INSERT INTO block (blocker, blocked) VALUES ($1, $2)`, blocker, a); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.InviteToParty(t.Context(), a, blocker, 4); !errors.Is(err, ErrBlocked) {
		t.Errorf("invite someone who blocked you = %v, want ErrBlocked", err)
	}

	first, notices, err := db.InviteToParty(t.Context(), a, b, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !noticesOf(notices, events.TypePartyInvite)[b] {
		t.Errorf("notices = %+v, want party.invite to b", notices)
	}
	again, _, err := db.InviteToParty(t.Context(), a, b, 4)
	if err != nil || again.ID != first.ID || !again.ExpiresAt.After(first.ExpiresAt.Add(-time.Second)) {
		t.Errorf("a repeated invite = %+v, %v; want the same invite with a refreshed expiry", again, err)
	}

	if _, _, err := db.AcceptInvite(t.Context(), b, first.ID, 4); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.InviteToParty(t.Context(), a, b, 4); !errors.Is(err, ErrAlreadyMember) {
		t.Errorf("invite a member = %v, want ErrAlreadyMember", err)
	}
	// Any member may invite, not only the leader.
	c := player(t, db)
	if _, _, err := db.InviteToParty(t.Context(), b, c, 4); err != nil {
		t.Errorf("invite by a member = %v, want it allowed", err)
	}
	// With the limit at 2, the party of two is full.
	if _, _, err := db.InviteToParty(t.Context(), a, c, 2); !errors.Is(err, ErrPartyFull) {
		t.Errorf("invite into a full party = %v, want ErrPartyFull", err)
	}
}

func TestInviteExpiryAndDecline(t *testing.T) {
	db := testDB(t)
	a, b, c := player(t, db), player(t, db), player(t, db)
	if _, _, err := db.CreateParty(t.Context(), a, 4, nil); err != nil {
		t.Fatal(err)
	}
	inv, _, err := db.InviteToParty(t.Context(), a, b, 4)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := db.AcceptInvite(t.Context(), c, inv.ID, 4); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("accepting someone else's invite = %v, want ErrInviteNotFound", err)
	}

	if _, err := db.Pool.Exec(t.Context(),
		`UPDATE party_invite SET expires_at = now() - interval '1 second' WHERE invite_id = $1`, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AcceptInvite(t.Context(), b, inv.ID, 4); !errors.Is(err, ErrInviteExpired) {
		t.Errorf("accept an expired invite = %v, want ErrInviteExpired", err)
	}
	if n, err := db.DeleteExpiredInvites(t.Context()); err != nil || n < 1 {
		t.Errorf("DeleteExpiredInvites = %d, %v; want at least the one", n, err)
	}
	if _, _, err := db.AcceptInvite(t.Context(), b, inv.ID, 4); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("accept a swept invite = %v, want ErrInviteNotFound", err)
	}

	fresh, _, err := db.InviteToParty(t.Context(), a, b, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeclineInvite(t.Context(), b, fresh.ID); err != nil {
		t.Errorf("decline = %v", err)
	}
	if err := db.DeclineInvite(t.Context(), b, fresh.ID); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("decline twice = %v, want ErrInviteNotFound", err)
	}
}
