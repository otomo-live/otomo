package events

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"uuid"
)

// testClient returns a client for the Valkey SESSION_TEST_VALKEY_URL names, or skips the
// test when it is unset — so a machine without Valkey still runs the tests that do not
// need one.
func testClient(t *testing.T) *Client {
	t.Helper()

	url := os.Getenv("SESSION_TEST_VALKEY_URL")
	if url == "" {
		t.Skip("SESSION_TEST_VALKEY_URL is not set; skipping the tests that need Valkey")
	}

	c, err := New(t.Context(), url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// newStream returns a player id no other test uses, so tests never read each other's
// events, and registers the cleanup that removes its keys. Every test that publishes needs
// this rather than a fixed id: a leftover list would make a sequence-number assertion fail
// on a rerun.
//
// The context is a fresh one rather than t.Context(), which is already cancelled by the
// time cleanup functions run.
func newStream(t *testing.T, c *Client) string {
	t.Helper()

	id := uuid.NewV7()
	player := id.String()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.vk.Do(ctx, c.vk.B().Del().Key(eventsKey(player), seqKey(player)).Build()).Error()
	})
	return player
}

func TestKnownType(t *testing.T) {
	for _, typ := range []string{
		TypeFriendRequest, TypeFriendAccepted, TypeFriendRemoved, TypePartyInvite,
		TypePartyUpdated, TypePartyKicked, TypePartyDisbanded, TypePresenceChange,
		TypePartyLaunching, TypePartyLaunchFailed, TypePartyReturned,
	} {
		if !KnownType(typ) {
			t.Errorf("KnownType(%q) = false", typ)
		}
	}
	// Near misses a typo would produce, and the empty string, which is what a caller that
	// forgot to pass a type sends.
	for _, typ := range []string{"", "friend", "friend.requested", "party", "presence.changed ", "Friend.Request"} {
		if KnownType(typ) {
			t.Errorf("KnownType(%q) = true", typ)
		}
	}
}

// TestPublishRejectsBeforeTouchingValkey uses a zero-value Client deliberately: both
// guards run before anything dereferences the connection, so the test also pins that a
// misconfigured caller cannot reach the network — and it needs no Valkey to run.
func TestPublishRejectsBeforeTouchingValkey(t *testing.T) {
	c := &Client{}

	if _, err := c.Publish(t.Context(), "player-1", "not.a.type", nil); err == nil {
		t.Error("Publish accepted an unknown event type")
	} else if !strings.Contains(err.Error(), "unknown event type") {
		t.Errorf("error = %v, want it to name the unknown type", err)
	}

	if _, err := c.Publish(t.Context(), "", TypeFriendRequest, nil); err == nil {
		t.Error("Publish accepted an empty player id")
	} else if !strings.Contains(err.Error(), "without a player id") {
		t.Errorf("error = %v, want it to say the player id is missing", err)
	}
}

func TestReadRejectsAnEmptyPlayerID(t *testing.T) {
	c := &Client{}
	if _, _, err := c.Read(t.Context(), "", 0); err == nil {
		t.Error("Read accepted an empty player id")
	}
}

func TestNewRejectsAMalformedURL(t *testing.T) {
	// No Valkey needed: a URL this package cannot parse is refused before any connection
	// is attempted.
	for _, bad := range []string{"not a url", "http://localhost:6379", "valkey://host/0/extra"} {
		t.Run(bad, func(t *testing.T) {
			c, err := New(t.Context(), bad)
			if err == nil {
				c.Close()
				t.Fatalf("New accepted %q", bad)
			}
			if !strings.Contains(err.Error(), "SESSION_VALKEY_URL") {
				t.Errorf("error = %v, want it to name the variable", err)
			}
		})
	}
}

func TestNewFailsWhenValkeyIsNotThere(t *testing.T) {
	// Port 1 is never a Valkey. valkey-go dials while building the client, so the failure
	// lands in New rather than on the first publish — which is the property that matters:
	// a container with a reachable URL and a dead server refuses to start instead of
	// accepting events it cannot store.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	c, err := New(ctx, "valkey://127.0.0.1:1/0")
	if err == nil {
		c.Close()
		t.Fatal("New accepted an unreachable Valkey")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error = %v, want it to name the address that failed", err)
	}
}

func TestPublishAssignsContiguousSequenceNumbers(t *testing.T) {
	c := testClient(t)
	player := newStream(t, c)

	for i, typ := range []string{TypeFriendRequest, TypeFriendAccepted, TypePartyUpdated} {
		ev, err := c.Publish(t.Context(), player, typ, map[string]int{"n": i})
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		if want := int64(i + 1); ev.Seq != want {
			t.Errorf("publish %d got seq %d, want %d", i, ev.Seq, want)
		}
		if ev.Type != typ {
			t.Errorf("publish %d got type %q, want %q", i, ev.Type, typ)
		}
		if ev.At == 0 {
			t.Errorf("publish %d has no timestamp", i)
		}
		if got, want := string(ev.Payload), fmt.Sprintf(`{"n":%d}`, i); got != want {
			t.Errorf("publish %d payload = %s, want %s", i, got, want)
		}
	}

	events, resync, err := c.Read(t.Context(), player, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if resync {
		t.Error("Read asked for a resync on a stream it has not fallen behind on")
	}
	if len(events) != 3 {
		t.Fatalf("read %d events, want 3", len(events))
	}
	for i, ev := range events {
		if want := int64(i + 1); ev.Seq != want {
			t.Errorf("event %d has seq %d, want %d", i, ev.Seq, want)
		}
	}
}

// TestPublishStoresAnEmptyPayloadAsAnObject pins the one bit of encoding a client depends
// on: an event with nothing to say carries {}, not null, so a handler can index into the
// payload without a nil check on every type.
func TestPublishStoresAnEmptyPayloadAsAnObject(t *testing.T) {
	c := testClient(t)
	player := newStream(t, c)

	ev, err := c.Publish(t.Context(), player, TypePresenceChange, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := string(ev.Payload); got != "{}" {
		t.Errorf("payload = %s, want {}", got)
	}

	events, _, err := c.Read(t.Context(), player, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("read back %d events, want 1", len(events))
	}
	if got := string(events[0].Payload); got != "{}" {
		t.Errorf("payload = %s, want {}", got)
	}
	if events[0].Time().IsZero() {
		t.Error("the event's timestamp did not survive the round trip")
	}
}

func TestReadReturnsOnlyEventsAfterTheCursor(t *testing.T) {
	c := testClient(t)
	player := newStream(t, c)

	for i := 0; i < 5; i++ {
		if _, err := c.Publish(t.Context(), player, TypePartyUpdated, map[string]int{"n": i}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	for _, tt := range []struct {
		after int64
		want  []int64
	}{
		{0, []int64{1, 2, 3, 4, 5}},
		{4, []int64{5}},
		{5, nil},
	} {
		events, resync, err := c.Read(t.Context(), player, tt.after)
		if err != nil {
			t.Fatalf("Read(after=%d): %v", tt.after, err)
		}
		if resync {
			t.Errorf("Read(after=%d) asked for a resync", tt.after)
		}
		var got []int64
		for _, ev := range events {
			got = append(got, ev.Seq)
		}
		if len(got) != len(tt.want) {
			t.Errorf("Read(after=%d) returned %v, want %v", tt.after, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("Read(after=%d) returned %v, want %v", tt.after, got, tt.want)
				break
			}
		}
	}

	// A cursor ahead of the counter belongs to a stream that expired and started again,
	// so NeedsResync tells the client to resync instead of waiting for numbers the new
	// stream already used.
	events, resync, err := c.Read(t.Context(), player, 99)
	if err != nil {
		t.Fatalf("Read(after=99): %v", err)
	}
	if !resync || len(events) != 0 {
		t.Errorf("Read(after=99) = %d events, resync %v; want none and a resync", len(events), resync)
	}
}

// TestReadTrimsToTheCapAndReportsAResync is §3.2's two limits at once: the list keeps the
// last EventCap events, and a client whose cursor is older than what survives is told so
// rather than handed a gap it cannot see.
//
// The cap is lowered for the test rather than publishing a hundred and one events, which
// is the reason it is an argument to the script and a field here.
func TestReadTrimsToTheCapAndReportsAResync(t *testing.T) {
	c := testClient(t)
	player := newStream(t, c)
	c.cap = 3

	for i := 0; i < 5; i++ {
		if _, err := c.Publish(t.Context(), player, TypePartyUpdated, nil); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	seq, err := c.Seq(t.Context(), player)
	if err != nil {
		t.Fatalf("Seq: %v", err)
	}
	if seq != 5 {
		t.Errorf("Seq = %d, want 5: trimming must not reset the counter", seq)
	}

	t.Run("the oldest retained sequence is derived, not stored", func(t *testing.T) {
		// after=1 is older than the oldest surviving event (5-3+1 = 3), so the client
		// missed something that was trimmed away.
		_, resync, err := c.Read(t.Context(), player, 1)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if !resync {
			t.Error("Read did not report a resync for a cursor older than the retained window")
		}
	})

	t.Run("a cursor at the boundary is served", func(t *testing.T) {
		events, resync, err := c.Read(t.Context(), player, 2)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if resync {
			t.Error("Read asked for a resync at the boundary, where nothing has been lost")
		}
		var got []int64
		for _, ev := range events {
			got = append(got, ev.Seq)
		}
		if len(got) != 3 || got[0] != 3 || got[2] != 5 {
			t.Errorf("Read = %v, want [3 4 5]", got)
		}
	})
}

func TestPublishSetsBothExpiries(t *testing.T) {
	c := testClient(t)
	player := newStream(t, c)

	if _, err := c.Publish(t.Context(), player, TypeFriendRequest, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Both keys expire, and the counter matters as much as the list: a counter that
	// outlived its list would make the derived "oldest retained" arithmetic answer for a
	// window that no longer exists.
	for _, key := range []string{eventsKey(player), seqKey(player)} {
		ttl, err := c.vk.Do(t.Context(), c.vk.B().Ttl().Key(key).Build()).AsInt64()
		if err != nil {
			t.Fatalf("TTL %s: %v", key, err)
		}
		if ttl <= 0 {
			t.Errorf("TTL(%s) = %d, want a positive expiry", key, ttl)
		}
		want := int64(c.ttl / time.Second)
		if ttl > want {
			t.Errorf("TTL(%s) = %d, want at most %d", key, ttl, want)
		}
		// A refreshed TTL, so a stream that keeps producing never expires mid-conversation.
		if ttl < want-5 {
			t.Errorf("TTL(%s) = %d, want about %d", key, ttl, want)
		}
	}
}

func TestAnEmptyStreamIsNotAnError(t *testing.T) {
	c := testClient(t)
	player := newStream(t, c)

	events, resync, err := c.Read(t.Context(), player, 0)
	if err != nil {
		t.Fatalf("Read on a stream that does not exist: %v", err)
	}
	if len(events) != 0 || resync {
		t.Errorf("Read = %v, resync=%v; want nothing and no resync", events, resync)
	}

	seq, err := c.Seq(t.Context(), player)
	if err != nil {
		t.Fatalf("Seq on a stream that does not exist: %v", err)
	}
	if seq != 0 {
		t.Errorf("Seq = %d, want 0", seq)
	}
}

// TestConcurrentProducersNeverShareASequenceNumber is SES-A4's whole reason for existing.
// The sequence number, the append, the trim and the expiry are one script executed
// atomically, so two producers racing on the same player get distinct numbers and the
// list never loses an event to a lost update.
//
// A read-modify-write built from separate commands would fail this test — not every run,
// which is why the assertion is on the exact set of numbers rather than on a count.
func TestConcurrentProducersNeverShareASequenceNumber(t *testing.T) {
	c := testClient(t)
	player := newStream(t, c)

	const (
		producers = 8
		perWorker = 25
		total     = producers * perWorker
	)
	// Above the total, so the trim cannot remove a number before it is checked.
	c.cap = total + 1

	var (
		mu   sync.Mutex
		seen = make(map[int64]int, total)
		errs []error
		wg   sync.WaitGroup
	)
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				ev, err := c.Publish(t.Context(), player, TypePartyUpdated, map[string]int{"p": p, "i": i})
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				} else {
					seen[ev.Seq]++
				}
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d publishes failed, first: %v", len(errs), errs[0])
	}
	if len(seen) != total {
		t.Errorf("%d distinct sequence numbers for %d events; %d were reused",
			len(seen), total, total-len(seen))
	}
	for seq, n := range seen {
		if n != 1 {
			t.Errorf("sequence number %d was handed out %d times", seq, n)
		}
		if seq < 1 || seq > total {
			t.Errorf("sequence number %d is outside 1..%d", seq, total)
		}
	}

	// And the stream holds every one of them, in order, which is the other half of the
	// claim: distinct numbers are worth nothing if the appends were lost.
	events, resync, err := c.Read(t.Context(), player, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if resync {
		t.Fatal("Read asked for a resync on a stream nothing was trimmed from")
	}
	if len(events) != total {
		t.Fatalf("the stream holds %d events, want %d", len(events), total)
	}
	for i, ev := range events {
		if want := int64(i + 1); ev.Seq != want {
			t.Errorf("event %d has seq %d, want %d", i, ev.Seq, want)
			break
		}
	}
}

// TestSeqReadsTheCounterWithoutTheList is the cheap call a fresh connection makes: it
// wants to know where the stream is so it can wait for the next event, without pulling the
// whole list to find out.
func TestSeqReadsTheCounterWithoutTheList(t *testing.T) {
	c := testClient(t)
	player := newStream(t, c)

	if seq, err := c.Seq(t.Context(), player); err != nil || seq != 0 {
		t.Fatalf("Seq = %d, %v; want 0, nil", seq, err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := c.Publish(t.Context(), player, TypePartyUpdated, nil); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if seq, err := c.Seq(t.Context(), player); err != nil || seq != int64(i) {
			t.Fatalf("after %d publishes, Seq = %d, %v; want %d, nil", i, seq, err, i)
		}
	}
}

func TestPingOnALiveServer(t *testing.T) {
	c := testClient(t)
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
