package presence

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

// fakeBackend keeps leases in a map and applies the same rate limit as the script.
type fakeBackend struct {
	mu       sync.Mutex
	status   map[string]string
	last     map[string]time.Time
	now      time.Time
	gone     []string
	count    int64
	trimErr  error
	countErr error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{status: map[string]string{}, last: map[string]time.Time{}, now: time.Unix(1_800_000_000, 0)}
}

func (f *fakeBackend) Heartbeat(_ context.Context, player, status string) (Beat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if last, ok := f.last[player]; ok && f.now.Sub(last) < MinInterval {
		return Beat{RetryAfter: MinInterval - f.now.Sub(last)}, nil
	}
	prev := f.status[player]
	if prev == "" {
		prev = StatusOffline
	}
	f.status[player], f.last[player] = status, f.now
	return Beat{Previous: prev}, nil
}

func (f *fakeBackend) Statuses(_ context.Context, players []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, p := range players {
		out[p] = StatusOffline
		if s := f.status[p]; s != "" {
			out[p] = s
		}
	}
	return out, nil
}

func (f *fakeBackend) Trim(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.gone {
		delete(f.status, p)
	}
	gone := f.gone
	f.gone = nil
	return gone, f.trimErr
}

func (f *fakeBackend) Count(context.Context) (int64, error) { return f.count, f.countErr }

type friendTable map[string][]string

func (t friendTable) AcceptedFriends(_ context.Context, p string) ([]string, error) { return t[p], nil }

type sent struct{ to, typ, player, status string }

type recorder struct {
	mu   sync.Mutex
	sent []sent
}

func (r *recorder) Publish(_ context.Context, to, typ string, payload any) (events.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := payload.(map[string]any)
	r.sent = append(r.sent, sent{to, typ, p["player_id"].(string), p["status"].(string)})
	return events.Event{}, nil
}

func newTestService(b Backend, f Friends, p Publisher) *Service {
	return NewService(b, f, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestAStatusChangeReachesOnlineFriendsOnly is SES-B3: a changed status is told to each
// accepted friend who is online, and to nobody else.
func TestAStatusChangeReachesOnlineFriendsOnly(t *testing.T) {
	b := newFakeBackend()
	b.status["friend-on"] = StatusOnline
	pub := &recorder{}
	s := newTestService(b, friendTable{"me": {"friend-on", "friend-off"}}, pub)

	if _, err := s.Heartbeat(t.Context(), "me", StatusOnline); err != nil {
		t.Fatal(err)
	}
	if len(pub.sent) != 1 || pub.sent[0] != (sent{"friend-on", events.TypePresenceChange, "me", StatusOnline}) {
		t.Errorf("sent %+v, want one presence.changed to the online friend", pub.sent)
	}

	// The same status again, after the interval, tells nobody.
	b.now = b.now.Add(20 * time.Second)
	if _, err := s.Heartbeat(t.Context(), "me", StatusOnline); err != nil {
		t.Fatal(err)
	}
	if len(pub.sent) != 1 {
		t.Errorf("an unchanged status was announced: %+v", pub.sent)
	}

	// A new status is.
	b.now = b.now.Add(20 * time.Second)
	if _, err := s.Heartbeat(t.Context(), "me", StatusAway); err != nil {
		t.Fatal(err)
	}
	if len(pub.sent) != 2 || pub.sent[1].status != StatusAway {
		t.Errorf("sent %+v, want away announced", pub.sent)
	}
}

// TestARefusedBeatChangesNothing is SES-B6 in the service: a beat under 10 s after the
// last is refused with how long to wait, and announces nothing.
func TestARefusedBeatChangesNothing(t *testing.T) {
	b := newFakeBackend()
	b.status["friend"] = StatusOnline
	pub := &recorder{}
	s := newTestService(b, friendTable{"me": {"friend"}}, pub)

	if _, err := s.Heartbeat(t.Context(), "me", StatusOnline); err != nil {
		t.Fatal(err)
	}
	b.now = b.now.Add(4 * time.Second)
	beat, err := s.Heartbeat(t.Context(), "me", StatusAway)
	if err != nil {
		t.Fatal(err)
	}
	if beat.RetryAfter != 6*time.Second {
		t.Errorf("RetryAfter = %v, want 6s", beat.RetryAfter)
	}
	if b.status["me"] != StatusOnline || len(pub.sent) != 1 {
		t.Errorf("a refused beat changed the status to %q or announced %+v", b.status["me"], pub.sent)
	}
}

// TestTickTellsFriendsWhoWentOfflineAndSetsTheGauge is SES-B4 and SES-B5.
func TestTickTellsFriendsWhoWentOfflineAndSetsTheGauge(t *testing.T) {
	b := newFakeBackend()
	b.status["me"], b.status["friend"] = StatusOnline, StatusOnline
	b.gone = []string{"me"}
	b.count = 42
	pub := &recorder{}
	s := newTestService(b, friendTable{"me": {"friend"}}, pub)

	s.Tick(t.Context())

	if len(pub.sent) != 1 || pub.sent[0] != (sent{"friend", events.TypePresenceChange, "me", StatusOffline}) {
		t.Errorf("sent %+v, want me offline told to friend", pub.sent)
	}
	if got := testutil.ToFloat64(s.online); got != 42 {
		t.Errorf("otomo_online_players = %v, want 42", got)
	}
}

func TestTickKeepsTheLastCountWhenValkeyFails(t *testing.T) {
	b := newFakeBackend()
	b.count = 7
	s := newTestService(b, nil, nil)
	s.Tick(t.Context())
	b.trimErr, b.countErr = errors.New("down"), errors.New("down")
	s.Tick(t.Context())
	if got := testutil.ToFloat64(s.online); got != 7 {
		t.Errorf("otomo_online_players = %v after a failed count, want the last good 7", got)
	}
}

func TestValidStatus(t *testing.T) {
	for _, s := range []string{StatusOnline, StatusInMenus, StatusAway} {
		if !ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = false", s)
		}
	}
	for _, s := range []string{"", StatusOffline, "Online", "in menus", "busy"} {
		if ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = true", s)
		}
	}
}
