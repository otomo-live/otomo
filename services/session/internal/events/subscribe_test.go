package events

import (
	"context"
	"testing"
	"time"
)

// These tests need Valkey (SESSION_TEST_VALKEY_URL) and skip without it.

// patternSubscriptions returns how many pattern subscriptions Valkey holds.
func patternSubscriptions(t *testing.T, c *Client) int64 {
	t.Helper()
	n, err := c.vk.Do(t.Context(), c.vk.B().PubsubNumpat().Build()).AsInt64()
	if err != nil {
		t.Fatalf("PUBSUB NUMPAT: %v", err)
	}
	return n
}

// subscribed starts instance's subscription into hub and waits until Valkey has it.
func subscribed(t *testing.T, instance, observer *Client, hub *Hub) {
	t.Helper()
	before := patternSubscriptions(t, observer)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		instance.Subscribe(ctx, hub, nil)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(5 * time.Second)
	for patternSubscriptions(t, observer) <= before {
		if time.Now().After(deadline) {
			t.Fatal("the subscription never reached Valkey")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAnEventOnInstanceBWakesAPollOnInstanceA is SE-4's cross-instance criterion. Two
// clients stand for two Session instances: the poll waits on instance A's hub and the
// event is produced through instance B's client. The wake-up must arrive within 100 ms.
func TestAnEventOnInstanceBWakesAPollOnInstanceA(t *testing.T) {
	a, b := testClient(t), testClient(t)
	player := newStream(t, a)
	hubA := NewHub()
	subscribed(t, a, b, hubA)

	w := hubA.Register(player)
	defer w.Release()
	// Subscribe wakes every waiter each time it subscribes; start from a quiet waiter.
	select {
	case <-w.Wake:
	default:
	}

	start := time.Now()
	if _, err := b.Publish(t.Context(), player, TypePartyInvite, map[string]string{"invite_id": "x"}); err != nil {
		t.Fatalf("publish on B: %v", err)
	}
	select {
	case <-w.Wake:
		if took := time.Since(start); took > 100*time.Millisecond {
			t.Errorf("A was woken %v after B published, want under 100 ms", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll on A was never woken by B's event")
	}

	evs, resync, err := a.Read(t.Context(), player, 0)
	if err != nil || resync || len(evs) != 1 || evs[0].Type != TypePartyInvite {
		t.Errorf("A read %+v, resync=%v, err=%v; want B's one event", evs, resync, err)
	}
}

// TestOnlyTheAddressedPlayerIsWoken: every instance hears every wake-up, and the hub
// passes on only the ones for its own waiters.
func TestOnlyTheAddressedPlayerIsWoken(t *testing.T) {
	a := testClient(t)
	p1, p2 := newStream(t, a), newStream(t, a)
	hub := NewHub()
	subscribed(t, a, a, hub)

	w2 := hub.Register(p2)
	defer w2.Release()
	select {
	case <-w2.Wake:
	default:
	}

	if _, err := a.Publish(t.Context(), p1, TypeFriendRequest, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w2.Wake:
		t.Error("p2 was woken by an event for p1")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSubscribeClosesTheHubWhenDone: shutting down ends the subscription and every
// held poll.
func TestSubscribeClosesTheHubWhenDone(t *testing.T) {
	a := testClient(t)
	hub := NewHub()
	w := hub.Register("held")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Subscribe(ctx, hub, nil)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not return after its context ended")
	}
	select {
	case <-w.Done:
	default:
		t.Error("the held poll was not ended")
	}
}

// TestReadAsksAnExpiredCursorToResync: a cursor from a stream that has since expired is
// ahead of the new stream, and must resync rather than wait for numbers already reused.
func TestReadAsksAnExpiredCursorToResync(t *testing.T) {
	c := testClient(t)
	player := newStream(t, c)

	if _, resync, err := c.Read(t.Context(), player, 41); err != nil || !resync {
		t.Errorf("Read(after=41) on an empty stream = resync %v, %v; want a resync", resync, err)
	}
	if _, err := c.Publish(t.Context(), player, TypePartyUpdated, nil); err != nil {
		t.Fatal(err)
	}
	if _, resync, err := c.Read(t.Context(), player, 41); err != nil || !resync {
		t.Errorf("Read(after=41) on a restarted stream = resync %v, %v; want a resync", resync, err)
	}
}
