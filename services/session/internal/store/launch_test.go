package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

// readyLobby makes a forming party of leader and one member, both ready.
func readyLobby(t *testing.T, db *DB) (leader, member string, p *Party) {
	t.Helper()
	leader, member = player(t, db), player(t, db)
	if _, _, err := db.CreateParty(t.Context(), leader, 4, seedSettings); err != nil {
		t.Fatal(err)
	}
	join(t, db, leader, member)
	if _, _, err := db.SetReady(t.Context(), leader, true); err != nil {
		t.Fatal(err)
	}
	p, _, err := db.SetReady(t.Context(), member, true)
	if err != nil {
		t.Fatal(err)
	}
	return leader, member, p
}

func TestStartLaunchNeedsTheLeaderEveryoneReadyAndTheRevision(t *testing.T) {
	db := testDB(t)
	leader, member, p := readyLobby(t, db)

	if _, _, _, _, err := db.StartLaunch(t.Context(), member, p.Revision, nil); !errors.Is(err, ErrNotLeader) {
		t.Errorf("launch by a member = %v, want ErrNotLeader", err)
	}
	var stale *RevisionMismatchError
	if _, _, _, _, err := db.StartLaunch(t.Context(), leader, p.Revision-1, nil); !errors.As(err, &stale) {
		t.Errorf("launch at a stale revision = %v", err)
	}
	refuse := errors.New("difficulty is not allowed")
	var bad *InvalidSettingsError
	if _, _, _, _, err := db.StartLaunch(t.Context(), leader, p.Revision, func(map[string]string) error { return refuse }); !errors.As(err, &bad) {
		t.Errorf("launch with settings the rules refuse = %v", err)
	}

	unready, _, err := db.SetReady(t.Context(), member, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := db.StartLaunch(t.Context(), leader, unready.Revision, nil); !errors.Is(err, ErrNotReady) {
		t.Errorf("launch with a member not ready = %v, want ErrNotReady", err)
	}
}

// TestTwoConcurrentPressesStartOneLaunch is LB-3's first criterion in the store: the
// party row lock lets exactly one press move forming to launching, and the other finds
// it launching and starts nothing.
func TestTwoConcurrentPressesStartOneLaunch(t *testing.T) {
	db := testDB(t)
	leader, _, p := readyLobby(t, db)

	var wg sync.WaitGroup
	start := make(chan struct{})
	started := make([]bool, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			<-start
			_, _, _, started[i], errs[i] = db.StartLaunch(t.Context(), leader, p.Revision, nil)
		})
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("press %d: %v", i, err)
		}
	}
	if started[0] == started[1] {
		t.Errorf("started = %v, want exactly one press to start the launch", started)
	}
	got, err := db.GetParty(t.Context(), leader)
	if err != nil || got.State != StateLaunching || got.Revision != p.Revision+1 {
		t.Errorf("party after two presses = %+v, %v; want launching, one revision later", got, err)
	}
}

func TestFinishLaunchOnceIntoInGame(t *testing.T) {
	db := testDB(t)
	leader, member, p := readyLobby(t, db)
	_, notices, members, started, err := db.StartLaunch(t.Context(), leader, p.Revision, nil)
	if err != nil || !started || len(members) != 2 {
		t.Fatalf("StartLaunch = %v %v %v", started, members, err)
	}
	if notices[0].Payload["state"] != StateLaunching {
		t.Errorf("party.updated state = %v, want launching", notices[0].Payload["state"])
	}

	alloc := uuid.NewV7().String()
	got, _, done, err := db.FinishLaunch(t.Context(), p.ID, alloc, "play.example.com", 27000)
	if err != nil || !done {
		t.Fatalf("FinishLaunch = %v %v", done, err)
	}
	if got.State != StateInGame || got.AllocationID != alloc || got.MatchAddress != "play.example.com" || got.MatchPort != 27000 {
		t.Errorf("after the launch = %+v", got)
	}

	// Duplicate finishers change nothing.
	if _, _, done, err := db.FinishLaunch(t.Context(), p.ID, uuid.NewV7().String(), "x", 1); err != nil || done {
		t.Errorf("second FinishLaunch = %v %v, want not done", done, err)
	}
	if _, _, done, err := db.FailLaunch(t.Context(), p.ID, "no_capacity"); err != nil || done {
		t.Errorf("FailLaunch after the finish = %v %v, want not done", done, err)
	}
	if _, _, _, started, err := db.StartLaunch(t.Context(), leader, got.Revision, nil); !errors.Is(err, ErrPartyLocked) || started {
		t.Errorf("launch while in_game = %v %v, want ErrPartyLocked", started, err)
	}
	_ = member
}

// TestAFailedLaunchIsFormingAgainWithReadyKept is doc 14 §7: the lobby is usable at
// once, and ready flags are kept so the leader can retry.
func TestAFailedLaunchIsFormingAgainWithReadyKept(t *testing.T) {
	db := testDB(t)
	leader, member, p := readyLobby(t, db)
	if _, _, _, _, err := db.StartLaunch(t.Context(), leader, p.Revision, nil); err != nil {
		t.Fatal(err)
	}
	got, notices, done, err := db.FailLaunch(t.Context(), p.ID, "no_capacity")
	if err != nil || !done {
		t.Fatalf("FailLaunch = %v %v", done, err)
	}
	if got.State != StateForming || !got.Members[0].Ready || !got.Members[1].Ready {
		t.Errorf("after the failure = %+v", got)
	}
	failed := noticesOf(notices, events.TypePartyLaunchFailed)
	if !failed[leader] || !failed[member] {
		t.Errorf("launch_failed went to %v, want both", failed)
	}
	// The leader can launch again straight away.
	if _, _, _, started, err := db.StartLaunch(t.Context(), leader, got.Revision, nil); err != nil || !started {
		t.Errorf("relaunch = %v %v", started, err)
	}
}

func TestStuckLaunchingFindsOnlyOldLaunches(t *testing.T) {
	db := testDB(t)
	leader, _, p := readyLobby(t, db)
	if _, _, _, _, err := db.StartLaunch(t.Context(), leader, p.Revision, nil); err != nil {
		t.Fatal(err)
	}

	has := func(list []Launching) bool {
		for _, l := range list {
			if l.PartyID == p.ID {
				return len(l.Members) == 2
			}
		}
		return false
	}
	fresh, err := db.StuckLaunching(t.Context(), 15*time.Second)
	if err != nil || has(fresh) {
		t.Errorf("a launch seconds old counted as stuck: %v %v", fresh, err)
	}
	if _, err := db.Pool.Exec(t.Context(),
		`UPDATE party SET state_changed_at = now() - interval '20 seconds' WHERE party_id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	stuck, err := db.StuckLaunching(t.Context(), 15*time.Second)
	if err != nil || !has(stuck) {
		t.Errorf("a launch 20 s old was not found: %v %v", stuck, err)
	}
}
