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

// matchStore keeps parties in a match, and returns one only while it is still in_game
// on the named allocation, as the real store's conditional update does.
type matchStore struct {
	mu      sync.Mutex
	members []string
	inGame  map[string]string // party -> allocation
	reasons map[string]string // party -> reason it was returned with
}

func newMatchStore(members []string, matches map[string]string) *matchStore {
	return &matchStore{members: members, inGame: matches, reasons: map[string]string{}}
}

func (s *matchStore) ReturnFromGame(_ context.Context, partyID, allocationID, reason string) (*store.Party, []store.Notice, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inGame[partyID] != allocationID || allocationID == "" {
		return nil, nil, false, nil
	}
	delete(s.inGame, partyID)
	s.reasons[partyID] = reason
	var notices []store.Notice
	for _, m := range s.members {
		notices = append(notices, store.Notice{PlayerID: m, Type: events.TypePartyReturned,
			Payload: map[string]any{"party_id": partyID, "reason": reason}})
	}
	return &store.Party{ID: partyID, State: store.StateForming}, notices, true, nil
}

func (s *matchStore) InGameLongerThan(context.Context, time.Duration) ([]store.InGame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.InGame
	for _, p := range []string{"party-1", "party-2", "party-3"} {
		if a, ok := s.inGame[p]; ok {
			out = append(out, store.InGame{PartyID: p, AllocationID: a})
		}
	}
	return out, nil
}

// fakeStatus answers GET /internal/allocations/{id} from a table, and counts calls.
type fakeStatus struct {
	mu     sync.Mutex
	status map[string]*allocator.Status
	errs   map[string]error
	calls  int
}

func (f *fakeStatus) Get(_ context.Context, id string) (*allocator.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := f.errs[id]; err != nil {
		return nil, err
	}
	return f.status[id], nil
}

func newReturner(s ReturnStore, st StatusReader, p Publisher) *Returner {
	return NewReturner(context.Background(), s, st, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestTheCallbackReturnsEveryMember: each member gets party.returned with the reason.
func TestTheCallbackReturnsEveryMember(t *testing.T) {
	s := newMatchStore([]string{"a", "b"}, map[string]string{"party-1": "alloc-1"})
	pub := &recorder{}
	r := newReturner(s, nil, pub)

	done, err := r.Return(t.Context(), "party-1", "alloc-1", "server_dead", SourceCallback)
	if err != nil || !done {
		t.Fatalf("Return = %v %v", done, err)
	}
	returned := pub.of(events.TypePartyReturned)
	if len(returned) != 2 || returned[0].Payload["reason"] != "server_dead" {
		t.Errorf("party.returned = %+v, want one per member with reason server_dead", returned)
	}

	// A repeated callback changes nothing and tells nobody.
	if done, err := r.Return(t.Context(), "party-1", "alloc-1", "server_dead", SourceCallback); err != nil || done {
		t.Errorf("second Return = %v %v, want not done", done, err)
	}
	if got := len(pub.of(events.TypePartyReturned)); got != 2 {
		t.Errorf("%d party.returned after a repeated callback, want 2", got)
	}
	if got := testutil.ToFloat64(r.returns.WithLabelValues(SourceCallback)); got != 1 {
		t.Errorf("session_return_total{source=callback} = %v, want 1", got)
	}
}

func TestAnEmptyReasonIsEnded(t *testing.T) {
	s := newMatchStore([]string{"a"}, map[string]string{"party-1": "alloc-1"})
	r := newReturner(s, nil, &recorder{})
	if _, err := r.Return(t.Context(), "party-1", "alloc-1", "", SourceCallback); err != nil {
		t.Fatal(err)
	}
	if s.reasons["party-1"] != ReasonEnded {
		t.Errorf("reason = %q, want ended", s.reasons["party-1"])
	}
}

// TestTheRepairPollReturnsAMissedCallback is LB-4's third criterion: with the callback
// missed, the poll finds the allocation no longer live and returns the party with the
// allocation's end_reason. A live allocation is left alone, and one the Allocator does
// not know is returned as ended.
func TestTheRepairPollReturnsAMissedCallback(t *testing.T) {
	s := newMatchStore([]string{"a", "b"}, map[string]string{
		"party-1": "alloc-ended",
		"party-2": "alloc-live",
		"party-3": "alloc-unknown",
	})
	st := &fakeStatus{
		status: map[string]*allocator.Status{
			"alloc-ended": {Status: "ended", EndReason: "expired"},
			"alloc-live":  {Status: "active"},
		},
		errs: map[string]error{"alloc-unknown": allocator.ErrNotFound},
	}
	pub := &recorder{}
	r := newReturner(s, st, pub)

	r.Repair(t.Context())

	if s.reasons["party-1"] != "expired" {
		t.Errorf("party-1 returned with %q, want the end_reason expired", s.reasons["party-1"])
	}
	if _, still := s.inGame["party-2"]; !still {
		t.Error("a party whose allocation is live was returned")
	}
	if s.reasons["party-3"] != ReasonEnded {
		t.Errorf("party-3 returned with %q, want ended for an allocation the Allocator does not know", s.reasons["party-3"])
	}
	if got := testutil.ToFloat64(r.returns.WithLabelValues(SourceRepair)); got != 2 {
		t.Errorf("session_return_total{source=repair} = %v, want 2", got)
	}
	if got := len(pub.of(events.TypePartyReturned)); got != 4 {
		t.Errorf("%d party.returned, want 2 members x 2 parties", got)
	}
}

// TestTheRepairPollStopsWhenTheAllocatorIsDown: an unreachable Allocator is not a
// reason to return anybody, and the round stops at the first error instead of waiting
// out one timeout per party.
func TestTheRepairPollStopsWhenTheAllocatorIsDown(t *testing.T) {
	s := newMatchStore([]string{"a"}, map[string]string{"party-1": "alloc-1", "party-2": "alloc-2"})
	st := &fakeStatus{errs: map[string]error{
		"alloc-1": errors.New("dial tcp: connection refused"),
		"alloc-2": errors.New("dial tcp: connection refused"),
	}}
	r := newReturner(s, st, &recorder{})

	r.Repair(t.Context())

	if len(s.inGame) != 2 {
		t.Errorf("parties returned while the Allocator was down: %v", s.reasons)
	}
	if st.calls != 1 {
		t.Errorf("%d Allocator calls, want the round to stop after the first failure", st.calls)
	}
}

// TestTheCallbackAndTheRepairRaceToOneReturn: whichever comes first returns the party,
// and the other changes nothing.
func TestTheCallbackAndTheRepairRaceToOneReturn(t *testing.T) {
	s := newMatchStore([]string{"a", "b"}, map[string]string{"party-1": "alloc-1"})
	st := &fakeStatus{status: map[string]*allocator.Status{"alloc-1": {Status: "ended", EndReason: "ended"}}}
	pub := &recorder{}
	r := newReturner(s, st, pub)

	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() { r.Repair(t.Context()) })
		wg.Go(func() { _, _ = r.Return(t.Context(), "party-1", "alloc-1", "ended", SourceCallback) })
	}
	wg.Wait()

	if got := len(pub.of(events.TypePartyReturned)); got != 2 {
		t.Errorf("%d party.returned, want one per member once", got)
	}
	total := testutil.ToFloat64(r.returns.WithLabelValues(SourceCallback)) + testutil.ToFloat64(r.returns.WithLabelValues(SourceRepair))
	if total != 1 {
		t.Errorf("%v returns counted, want 1", total)
	}
}

func TestWithoutAnAllocatorTheRepairPollIsOff(t *testing.T) {
	s := newMatchStore([]string{"a"}, map[string]string{"party-1": "alloc-1"})
	r := newReturner(s, nil, &recorder{})
	r.Repair(t.Context())
	if len(s.inGame) != 1 {
		t.Error("the repair poll returned a party with no Allocator to ask")
	}
}
