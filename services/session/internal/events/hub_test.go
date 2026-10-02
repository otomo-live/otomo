package events

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// These tests need no Valkey: the Hub is the fan-out and the replacement logic, and the
// subscription only feeds it Notify calls.

func woken(w *Waiter) bool {
	select {
	case <-w.Wake:
		return true
	case <-time.After(time.Second):
		return false
	}
}

func ended(w *Waiter) bool {
	select {
	case <-w.Done:
		return true
	case <-time.After(time.Second):
		return false
	}
}

func TestNotifyWakesOnlyThatPlayer(t *testing.T) {
	h := NewHub()
	a, b := h.Register("a"), h.Register("b")
	defer a.Release()
	defer b.Release()

	h.Notify("a")
	if !woken(a) {
		t.Error("a was not woken")
	}
	select {
	case <-b.Wake:
		t.Error("b was woken by a's notification")
	default:
	}
	h.Notify("nobody") // no waiter: nothing happens, and nothing blocks
}

// TestNotifyNeverBlocks: the subscription's reader calls Notify, so it must return even
// when the poll has not taken the previous wake-up yet. The wake-ups fold into one.
func TestNotifyNeverBlocks(t *testing.T) {
	h := NewHub()
	w := h.Register("a")
	defer w.Release()

	done := make(chan struct{})
	go func() {
		for range 1000 {
			h.Notify("a")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked on a waiter that was not reading")
	}
	if !woken(w) {
		t.Fatal("no wake-up was kept")
	}
	select {
	case <-w.Wake:
		t.Error("1000 notifications left more than one wake-up pending")
	default:
	}
}

// TestANewerPollReplacesTheOlder is SES-E3: one poll per player per instance, and the
// newer one wins.
func TestANewerPollReplacesTheOlder(t *testing.T) {
	h := NewHub()
	old := h.Register("a")
	newer := h.Register("a")
	defer newer.Release()

	if !ended(old) {
		t.Fatal("the older poll was not ended")
	}
	select {
	case <-newer.Done:
		t.Fatal("the newer poll was ended too")
	default:
	}

	// The replaced poll releasing itself must not remove its replacement.
	old.Release()
	if h.Waiting() != 1 {
		t.Fatalf("Waiting() = %d after the old poll released, want 1", h.Waiting())
	}
	h.Notify("a")
	if !woken(newer) {
		t.Error("the newer poll no longer receives wake-ups")
	}
}

func TestReleaseRemovesTheWaiter(t *testing.T) {
	h := NewHub()
	w := h.Register("a")
	w.Release()
	w.Release() // twice is fine
	if h.Waiting() != 0 {
		t.Errorf("Waiting() = %d, want 0", h.Waiting())
	}
}

func TestNotifyAllWakesEveryone(t *testing.T) {
	h := NewHub()
	ws := []*Waiter{h.Register("a"), h.Register("b"), h.Register("c")}
	h.NotifyAll()
	for i, w := range ws {
		if !woken(w) {
			t.Errorf("waiter %d was not woken", i)
		}
		w.Release()
	}
}

func TestCloseEndsEveryPollAndRefusesNewOnes(t *testing.T) {
	h := NewHub()
	a, b := h.Register("a"), h.Register("b")
	h.Close()
	if !ended(a) || !ended(b) {
		t.Fatal("Close left a poll held")
	}
	late := h.Register("c")
	if !ended(late) {
		t.Error("a poll registered after Close was not ended at once")
	}
	if h.Waiting() != 0 {
		t.Errorf("Waiting() = %d after Close, want 0", h.Waiting())
	}
}

// TestHubLeavesNoGoroutines: the Hub starts none of its own, so registering, replacing
// and releasing many polls must leave the goroutine count where it was.
func TestHubLeavesNoGoroutines(t *testing.T) {
	h := NewHub()
	before := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := range 500 {
		wg.Go(func() {
			w := h.Register(string(rune('a' + i%26)))
			h.Notify(w.player)
			select {
			case <-w.Wake:
			case <-w.Done:
			}
			w.Release()
		})
	}
	wg.Wait()

	if h.Waiting() != 0 {
		t.Errorf("Waiting() = %d, want 0", h.Waiting())
	}
	if after := waitGoroutines(before); after > before {
		t.Errorf("goroutines went from %d to %d", before, after)
	}
}

// waitGoroutines gives exiting goroutines a moment to finish and returns the count.
func waitGoroutines(target int) int {
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= target || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNeedsResync(t *testing.T) {
	for _, tt := range []struct {
		after, seq, length int64
		want               bool
	}{
		{0, 0, 0, false}, // a new player, nothing sent yet
		{0, 3, 3, false}, // from the start of a full window
		{3, 3, 3, false}, // up to date
		{2, 5, 3, false}, // at the boundary: 3, 4 and 5 are retained
		{1, 5, 3, true},  // 2 was trimmed away
		{41, 0, 0, true}, // the stream expired; the cursor belongs to the old one
		{41, 2, 2, true}, // the stream expired and restarted at 1
		{6, 5, 3, true},  // a cursor ahead of the stream
		{0, 150, 100, true},
	} {
		if got := NeedsResync(tt.after, tt.seq, tt.length); got != tt.want {
			t.Errorf("NeedsResync(after=%d, seq=%d, length=%d) = %v, want %v",
				tt.after, tt.seq, tt.length, got, tt.want)
		}
	}
}
