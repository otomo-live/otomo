package launch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/otomo-live/otomo/services/session/internal/allocator"
	"github.com/otomo-live/otomo/services/session/internal/events"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

// fakeStore keeps one party's state, and finishes a launch only while it is launching,
// as the real store does under its row lock.
type fakeStore struct {
	mu       sync.Mutex
	state    string
	members  []string
	finishes int
	failures []string
	stuck    []store.Launching
}

func (s *fakeStore) party() *store.Party {
	p := &store.Party{ID: "party-1", State: s.state, Revision: 5}
	for _, m := range s.members {
		p.Members = append(p.Members, store.Member{PlayerID: m})
	}
	return p
}

func (s *fakeStore) FinishLaunch(_ context.Context, _, _, _ string, _ int) (*store.Party, []store.Notice, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != store.StateLaunching {
		return nil, nil, false, nil
	}
	s.state = store.StateInGame
	s.finishes++
	p := s.party()
	return p, []store.Notice{{PlayerID: s.members[0], Type: events.TypePartyUpdated}}, true, nil
}

func (s *fakeStore) FailLaunch(_ context.Context, _, reason string) (*store.Party, []store.Notice, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != store.StateLaunching {
		return nil, nil, false, nil
	}
	s.state = store.StateForming
	s.failures = append(s.failures, reason)
	var notices []store.Notice
	for _, m := range s.members {
		notices = append(notices, store.Notice{PlayerID: m, Type: events.TypePartyLaunchFailed, Payload: map[string]any{"reason": reason}})
	}
	return s.party(), notices, true, nil
}

func (s *fakeStore) StuckLaunching(context.Context, time.Duration) ([]store.Launching, error) {
	return s.stuck, nil
}

// fakeAllocator is idempotent per party, like the Allocator: the same party always gets
// the same allocation, with fresh tickets.
type fakeAllocator struct {
	mu     sync.Mutex
	err    error
	calls  int
	byID   map[string]string
	serial int
}

func (a *fakeAllocator) Allocate(_ context.Context, partyID string, players []string) (*allocator.Allocation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	if a.byID == nil {
		a.byID = map[string]string{}
	}
	id, ok := a.byID[partyID]
	if !ok {
		a.serial++
		id = "alloc-" + string(rune('0'+a.serial))
		a.byID[partyID] = id
	}
	tickets := map[string]string{}
	for _, p := range players {
		tickets[p] = "ticket-for-" + p
	}
	return &allocator.Allocation{AllocationID: id, Address: "play.example.com", Port: 27000, Tickets: tickets}, nil
}

type recorder struct {
	mu   sync.Mutex
	sent []store.Notice
}

func (r *recorder) Publish(_ context.Context, player, typ string, payload any) (events.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, _ := payload.(map[string]any)
	r.sent = append(r.sent, store.Notice{PlayerID: player, Type: typ, Payload: p})
	return events.Event{}, nil
}

func (r *recorder) of(typ string) []store.Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []store.Notice
	for _, n := range r.sent {
		if n.Type == typ {
			out = append(out, n)
		}
	}
	return out
}

func newLauncher(s Store, a Allocator, p Publisher) *Launcher {
	return New(context.Background(), s, a, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestEachMemberGetsOnlyTheirOwnTicket is doc 14 §3 and §7: party.launching is sent to
// each member individually, with only their own ticket.
func TestEachMemberGetsOnlyTheirOwnTicket(t *testing.T) {
	s := &fakeStore{state: store.StateLaunching, members: []string{"a", "b", "c"}}
	pub := &recorder{}
	l := newLauncher(s, &fakeAllocator{}, pub)

	if got := l.Launch(t.Context(), "party-1", s.members); got != "in_game" {
		t.Fatalf("Launch = %s, want in_game", got)
	}
	launching := pub.of(events.TypePartyLaunching)
	if len(launching) != 3 {
		t.Fatalf("%d party.launching events, want one per member", len(launching))
	}
	for _, n := range launching {
		if n.Payload["ticket"] != "ticket-for-"+n.PlayerID {
			t.Errorf("%s got ticket %v, want only their own", n.PlayerID, n.Payload["ticket"])
		}
		for _, key := range []string{"party_id", "revision", "allocation_id", "address", "port", "ticket_expires_at"} {
			if _, ok := n.Payload[key]; !ok {
				t.Errorf("party.launching for %s has no %s", n.PlayerID, key)
			}
		}
	}
	if len(pub.of(events.TypePartyUpdated)) == 0 {
		t.Error("no party.updated for the move to in_game")
	}
	if got := testutil.ToFloat64(l.results.WithLabelValues("in_game")); got != 1 {
		t.Errorf("session_launch_total{result=in_game} = %v", got)
	}
}

// TestNoCapacityFailsTheLaunchForEveryone is LB-3's second criterion: with no game
// server, every member gets party.launch_failed{no_capacity} and the lobby is forming
// again, usable at once.
func TestNoCapacityFailsTheLaunchForEveryone(t *testing.T) {
	s := &fakeStore{state: store.StateLaunching, members: []string{"a", "b"}}
	pub := &recorder{}
	l := newLauncher(s, &fakeAllocator{err: allocator.ErrNoCapacity}, pub)

	if got := l.Launch(t.Context(), "party-1", s.members); got != ReasonNoCapacity {
		t.Fatalf("Launch = %s, want no_capacity", got)
	}
	failed := pub.of(events.TypePartyLaunchFailed)
	if len(failed) != 2 || failed[0].Payload["reason"] != ReasonNoCapacity {
		t.Errorf("launch_failed = %+v, want one per member with reason no_capacity", failed)
	}
	if s.state != store.StateForming {
		t.Errorf("state after no_capacity = %s, want forming", s.state)
	}
}

func TestAnyOtherFailureIsAllocatorUnavailable(t *testing.T) {
	for _, err := range []error{allocator.ErrUnexpectedAnswer, context.DeadlineExceeded, allocator.ErrConflict, errors.New("dial tcp: refused")} {
		s := &fakeStore{state: store.StateLaunching, members: []string{"a"}}
		l := newLauncher(s, &fakeAllocator{err: err}, &recorder{})
		if got := l.Launch(t.Context(), "party-1", s.members); got != ReasonAllocatorUnavailable {
			t.Errorf("%v = %s, want allocator_unavailable", err, got)
		}
		if s.failures[0] != ReasonAllocatorUnavailable {
			t.Errorf("%v recorded %v", err, s.failures)
		}
	}
}

// TestARestartMidLaunchEndsInOneAllocation is LB-3's third criterion: the launch call
// was lost with a restart, so the sweep repeats it; the original call racing the sweep
// changes nothing twice. The Allocator is idempotent per party, and only the first
// finisher commits.
func TestARestartMidLaunchEndsInOneAllocation(t *testing.T) {
	s := &fakeStore{state: store.StateLaunching, members: []string{"a", "b"}}
	s.stuck = []store.Launching{{PartyID: "party-1", Members: s.members}}
	alloc := &fakeAllocator{}
	pub := &recorder{}
	l := newLauncher(s, alloc, pub)

	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() { l.Sweep(t.Context()) })
	}
	wg.Go(func() { l.Launch(t.Context(), "party-1", s.members) })
	wg.Wait()

	if len(alloc.byID) != 1 {
		t.Errorf("%d distinct allocations, want 1", len(alloc.byID))
	}
	if s.finishes != 1 {
		t.Errorf("the launch was finished %d times, want 1", s.finishes)
	}
	if got := len(pub.of(events.TypePartyLaunching)); got != 2 {
		t.Errorf("%d party.launching events, want one per member once", got)
	}
	if got := testutil.ToFloat64(l.results.WithLabelValues("superseded")); got != 3 {
		t.Errorf("superseded finishers = %v, want 3", got)
	}
}

func TestStartRunsInTheBackground(t *testing.T) {
	s := &fakeStore{state: store.StateLaunching, members: []string{"a"}}
	l := newLauncher(s, &fakeAllocator{}, &recorder{})
	l.Start("party-1", s.members)
	l.Wait()
	if s.state != store.StateInGame {
		t.Errorf("state after Start = %s, want in_game", s.state)
	}
}
