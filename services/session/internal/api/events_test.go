package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/events"
)

// fakeStream is an in-memory event stream with the same cursor rules as the real one. A
// test publishes into it and, like the Lua producer, wakes the player's poll.
type fakeStream struct {
	mu     sync.Mutex
	events []events.Event
	hub    *events.Hub
	err    error
	reads  int
}

func (f *fakeStream) Read(_ context.Context, _ string, after int64) ([]events.Event, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return nil, false, f.err
	}
	seq := int64(len(f.events))
	if events.NeedsResync(after, seq, seq) {
		return nil, true, nil
	}
	var out []events.Event
	for _, ev := range f.events {
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out, false, nil
}

func (f *fakeStream) publish(player, typ string) {
	f.mu.Lock()
	f.events = append(f.events, events.Event{Seq: int64(len(f.events) + 1), Type: typ, Payload: json.RawMessage(`{}`)})
	f.mu.Unlock()
	f.hub.Notify(player)
}

func newPoller(hold time.Duration) (*Handlers, *fakeStream) {
	hub := events.NewHub()
	stream := &fakeStream{hub: hub}
	return &Handlers{Events: stream, Hub: hub, EventHold: hold}, stream
}

// poll runs GET /events?after=<after> for testPlayer with ctx and returns the recorder
// once the handler has answered.
func poll(h *Handlers, ctx context.Context, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, PlayerPrefix+"/events"+query, nil)
	req = req.WithContext(auth.WithIdentity(ctx, &auth.Identity{Subject: testPlayer.String()}))
	rec := httptest.NewRecorder()
	h.For(Route{Method: http.MethodGet, Path: PlayerPrefix + "/events"}).ServeHTTP(rec, req)
	return rec
}

func decodeEvents(t *testing.T, rec *httptest.ResponseRecorder) []events.Event {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body)
	}
	var evs []events.Event
	if err := json.Unmarshal(rec.Body.Bytes(), &evs); err != nil {
		t.Fatalf("body %q is not an event list: %v", rec.Body, err)
	}
	return evs
}

func TestPollAnswersAtOnceWhenEventsWait(t *testing.T) {
	h, stream := newPoller(time.Minute)
	stream.publish(testPlayer.String(), events.TypePartyUpdated)
	stream.publish(testPlayer.String(), events.TypePartyInvite)

	start := time.Now()
	evs := decodeEvents(t, poll(h, t.Context(), "?after=1"))
	if len(evs) != 1 || evs[0].Seq != 2 {
		t.Errorf("events = %+v, want only seq 2", evs)
	}
	if time.Since(start) > time.Second {
		t.Error("the poll held although an event was waiting")
	}
}

func TestPollHoldExpiresWithAnEmptyList(t *testing.T) {
	h, _ := newPoller(100 * time.Millisecond)
	start := time.Now()
	rec := poll(h, t.Context(), "?after=0")
	if rec.Body.String() != "[]\n" {
		t.Errorf("body = %q, want []", rec.Body)
	}
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Errorf("answered after %v, before the hold ended", took)
	}
}

// TestPollIsWokenByAPublish: an event produced while the poll is held is delivered at
// once, not at the end of the hold.
func TestPollIsWokenByAPublish(t *testing.T) {
	h, stream := newPoller(time.Minute)

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- poll(h, context.Background(), "?after=0") }()
	waitForWaiters(t, h.Hub, 1)

	published := time.Now()
	stream.publish(testPlayer.String(), events.TypeFriendRequest)
	select {
	case rec := <-done:
		if took := time.Since(published); took > 100*time.Millisecond {
			t.Errorf("the poll answered %v after the publish, want under 100 ms", took)
		}
		if evs := decodeEvents(t, rec); len(evs) != 1 || evs[0].Type != events.TypeFriendRequest {
			t.Errorf("events = %+v", evs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll was not woken")
	}
}

func TestPollAsksForAResync(t *testing.T) {
	h, _ := newPoller(time.Minute)
	// The stream is empty (seq 0) but the client has a cursor from an older one.
	rec := poll(h, t.Context(), "?after=41")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"resync":true}`+"\n" {
		t.Errorf("poll = %d %q, want 200 {\"resync\":true}", rec.Code, rec.Body)
	}
}

// TestANewerPollEndsTheOlderWithAnEmptyList is SES-E3 through the handler.
func TestANewerPollEndsTheOlderWithAnEmptyList(t *testing.T) {
	h, _ := newPoller(time.Minute)

	first := make(chan *httptest.ResponseRecorder)
	go func() { first <- poll(h, context.Background(), "?after=0") }()
	waitForWaiters(t, h.Hub, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := make(chan *httptest.ResponseRecorder)
	go func() { second <- poll(h, ctx, "?after=0") }()

	select {
	case rec := <-first:
		if rec.Body.String() != "[]\n" {
			t.Errorf("the replaced poll answered %q, want []", rec.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the older poll was not ended by the newer one")
	}
	if n := h.Hub.Waiting(); n != 1 {
		t.Errorf("Waiting() = %d, want only the newer poll", n)
	}
	cancel()
	<-second
}

func TestPollRejectsABadCursor(t *testing.T) {
	h, _ := newPoller(time.Minute)
	for _, q := range []string{"?after=-1", "?after=abc", "?after=1.5"} {
		if rec := poll(h, t.Context(), q); rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_after" {
			t.Errorf("GET /events%s = %d %s, want 400 invalid_after", q, rec.Code, rec.Body)
		}
	}
	// No cursor means 0.
	h.EventHold = 10 * time.Millisecond
	if rec := poll(h, t.Context(), ""); rec.Code != http.StatusOK {
		t.Errorf("GET /events with no cursor = %d, want 200", rec.Code)
	}
}

func TestPollReadFailureIs500(t *testing.T) {
	h, stream := newPoller(time.Minute)
	stream.err = errors.New("valkey down")
	if rec := poll(h, t.Context(), "?after=0"); rec.Code != http.StatusInternalServerError {
		t.Errorf("poll with a failing read = %d, want 500", rec.Code)
	}
	if h.Hub.Waiting() != 0 {
		t.Error("a failed poll stayed registered")
	}
}

func TestShutdownEndsHeldPolls(t *testing.T) {
	h, _ := newPoller(time.Minute)
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- poll(h, context.Background(), "?after=0") }()
	waitForWaiters(t, h.Hub, 1)

	h.Hub.Close()
	select {
	case rec := <-done:
		if rec.Body.String() != "[]\n" {
			t.Errorf("poll ended by shutdown answered %q, want []", rec.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the held poll")
	}
}

// TestPollsLeaveNoGoroutines: polls that time out, are replaced, are woken or whose
// client leaves all give their goroutine back and leave no waiter behind.
func TestPollsLeaveNoGoroutines(t *testing.T) {
	h, stream := newPoller(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch i % 4 {
			case 0: // times out
			case 1: // the client leaves
				go func() { time.Sleep(10 * time.Millisecond); cancel() }()
			case 2: // woken
				go func() {
					time.Sleep(10 * time.Millisecond)
					stream.publish(testPlayer.String(), events.TypePartyUpdated)
				}()
			case 3: // replaced by the next poll: all 200 share one player
			}
			poll(h, ctx, "?after=1000000")
		})
	}
	wg.Wait()

	if n := h.Hub.Waiting(); n != 0 {
		t.Errorf("Waiting() = %d after every poll answered, want 0", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines went from %d to %d", before, after)
	}
}

func waitForWaiters(t *testing.T, hub *events.Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for hub.Waiting() != n {
		if time.Now().After(deadline) {
			t.Fatalf("Waiting() = %d, want %d", hub.Waiting(), n)
		}
		time.Sleep(time.Millisecond)
	}
}
