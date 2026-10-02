// The read side of the long-poll (SE-4; design/04-session-minimal.md §4, §5a
// SES-E2 and SES-E3): one pub/sub subscription per Session instance, fanned out to the
// polls waiting on this instance.

package events

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/valkey-io/valkey-go"
)

// notifyPattern is the one subscription an instance holds. Every producer, on any
// instance, publishes to notify:{player}, so every instance hears every wake-up and
// passes on the ones for players it has a poll for.
const notifyPattern = "notify:*"

// resubscribeDelay is the pause before subscribing again after the subscription fails,
// so a Valkey that is down is not hammered.
const resubscribeDelay = time.Second

// Waiter is one held poll's registration with the Hub.
type Waiter struct {
	// Wake receives a value when the player's stream may have changed. It is buffered
	// with room for one, so a wake-up that arrives while the poll is reading is kept and
	// not lost, and several are folded into one re-read.
	Wake <-chan struct{}

	// Done is closed when this poll must end now with an empty answer: a newer poll for
	// the same player replaced it, or the instance is shutting down.
	Done <-chan struct{}

	hub    *Hub
	player string
	wake   chan struct{}
	done   chan struct{}
	once   sync.Once
}

// end closes Done once.
func (w *Waiter) end() {
	w.once.Do(func() { close(w.done) })
}

// Release removes the waiter from the Hub. Call it when the poll answers, however it
// answers; a deferred Release right after Register is the pattern. It is safe to call
// after the waiter was replaced, and more than once.
func (w *Waiter) Release() {
	w.hub.mu.Lock()
	if w.hub.waiters[w.player] == w {
		delete(w.hub.waiters, w.player)
	}
	w.hub.mu.Unlock()
	w.end()
}

// Hub holds at most one waiter per player on this instance and wakes them. Safe for
// concurrent use. The zero value is not usable; call NewHub.
type Hub struct {
	mu      sync.Mutex
	waiters map[string]*Waiter
	closed  bool
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{waiters: map[string]*Waiter{}}
}

// Register adds a waiter for player. A poll already held for the same player on this
// instance is ended first (its Done closes), so one player never holds two polls here
// (SES-E3). A poll held on another instance is ended by its own instance only; the
// gateway sends a player's requests wherever it likes, and replacing across instances
// would need a broadcast for a case the client's single loop already avoids.
//
// After Close, Register returns a waiter that is already done.
func (h *Hub) Register(player string) *Waiter {
	w := &Waiter{
		hub:    h,
		player: player,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	w.Wake, w.Done = w.wake, w.done

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		w.end()
		return w
	}
	if old := h.waiters[player]; old != nil {
		old.end()
	}
	h.waiters[player] = w
	return w
}

// Notify wakes player's waiter on this instance, if there is one. It never blocks: the
// subscription's reader calls it, and a slow poll must not hold up every other player's
// wake-ups.
func (h *Hub) Notify(player string) {
	h.mu.Lock()
	w := h.waiters[player]
	h.mu.Unlock()
	if w != nil {
		wake(w)
	}
}

// NotifyAll wakes every waiter. The subscriber calls it each time it (re)subscribes,
// because a wake-up published while there was no subscription is lost, and a re-read
// is cheap.
func (h *Hub) NotifyAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.waiters {
		wake(w)
	}
}

// Waiting reports how many polls are registered. It is for metrics and tests.
func (h *Hub) Waiting() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.waiters)
}

// Close ends every held poll and refuses new ones, so a shutting-down instance answers
// its polls at once instead of making the server's shutdown wait out a 25 s hold.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for player, w := range h.waiters {
		w.end()
		delete(h.waiters, player)
	}
}

func wake(w *Waiter) {
	select {
	case w.wake <- struct{}{}:
	default:
		// A wake-up is already pending; the poll re-reads the whole stream anyway.
	}
}

// Subscribe holds this instance's one pub/sub subscription (SES-E2) and feeds it to hub
// until ctx is done, then closes the hub. It subscribes again after any failure, and
// wakes every waiter each time, since wake-ups sent while unsubscribed are lost.
//
// Run it in its own goroutine for the life of the process.
func (c *Client) Subscribe(ctx context.Context, hub *Hub, log *slog.Logger) {
	defer hub.Close()

	cmd := c.vk.B().Psubscribe().Pattern(notifyPattern).Build()
	for {
		hub.NotifyAll()
		err := c.vk.Receive(ctx, cmd, func(m valkey.PubSubMessage) {
			if player, ok := strings.CutPrefix(m.Channel, "notify:"); ok {
				hub.Notify(player)
			}
		})
		if ctx.Err() != nil || errors.Is(err, valkey.ErrClosing) {
			return
		}
		if log != nil {
			log.Warn("event subscription ended; subscribing again",
				slog.Any("error", err), slog.Duration("after", resubscribeDelay))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(resubscribeDelay):
		}
	}
}
