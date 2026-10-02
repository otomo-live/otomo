// Package launch runs the part of a lobby launch that happens after StartLaunch has
// committed launching (LB-3; design/14-launch-handoff.md §3, §6, §7): asking
// the Allocator for a game server, committing the outcome, and telling the members.
//
// The Allocator is called outside any transaction, with its own 5 s deadline. The
// outcome is committed by store.FinishLaunch or store.FailLaunch, which act only while
// the party is still launching, so the sweep that repeats a lost launch can race the
// original without a second outcome.
//
// The Returner (LB-4) is the other end of a match: the Allocator's callback
// and the missed-callback repair poll both return a party to forming through it.
package launch

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/otomo-live/otomo/services/session/internal/allocator"
	"github.com/otomo-live/otomo/services/session/internal/events"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

// Doc 14 §6's stuck-launch sweep: every 10 s, parties launching for more than 15 s.
const (
	SweepInterval = 10 * time.Second
	StuckAfter    = 15 * time.Second
)

// Launch failure reasons (doc 14 §7).
const (
	ReasonNoCapacity           = "no_capacity"
	ReasonAllocatorUnavailable = "allocator_unavailable"
)

// Store is what the launcher needs from the party store. *store.DB implements it.
type Store interface {
	FinishLaunch(ctx context.Context, partyID, allocationID, address string, port int) (*store.Party, []store.Notice, bool, error)
	FailLaunch(ctx context.Context, partyID, reason string) (*store.Party, []store.Notice, bool, error)
	StuckLaunching(ctx context.Context, olderThan time.Duration) ([]store.Launching, error)
}

// Allocator is what the launcher needs from the Allocator client.
type Allocator interface {
	Allocate(ctx context.Context, partyID string, playerIDs []string) (*allocator.Allocation, error)
}

// Publisher sends one event to a player's stream. *events.Client implements it.
type Publisher interface {
	Publish(ctx context.Context, playerID, typ string, payload any) (events.Event, error)
}

// Launcher finishes launches. Build it with New.
type Launcher struct {
	ctx   context.Context
	store Store
	alloc Allocator
	pub   Publisher
	log   *slog.Logger

	results *prometheus.CounterVec
	wg      sync.WaitGroup
}

// New returns a Launcher whose background launches stop when ctx ends.
func New(ctx context.Context, s Store, a Allocator, p Publisher, log *slog.Logger) *Launcher {
	if log == nil {
		log = slog.Default()
	}
	l := &Launcher{
		ctx: ctx, store: s, alloc: a, pub: p, log: log,
		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "session_launch_total",
			Help: "Lobby launches finished, by result (in_game, no_capacity, allocator_unavailable, superseded).",
		}, []string{"result"}),
	}
	for _, r := range []string{"in_game", ReasonNoCapacity, ReasonAllocatorUnavailable, "superseded"} {
		l.results.WithLabelValues(r)
	}
	return l
}

// Collectors returns the launcher's metrics, for the service's registry.
func (l *Launcher) Collectors() []prometheus.Collector {
	return []prometheus.Collector{l.results}
}

// Start finishes the launch of partyID for members in the background. The request that
// committed launching has already been answered (202).
func (l *Launcher) Start(partyID string, members []string) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.Launch(l.ctx, partyID, members)
	}()
}

// Wait blocks until every background launch has finished.
func (l *Launcher) Wait() { l.wg.Wait() }

// Launch asks the Allocator for a game server for partyID and commits the outcome. It
// returns the result it counted.
func (l *Launcher) Launch(ctx context.Context, partyID string, members []string) string {
	a, err := l.alloc.Allocate(ctx, partyID, members)
	if err != nil {
		reason := ReasonAllocatorUnavailable
		if errors.Is(err, allocator.ErrNoCapacity) {
			reason = ReasonNoCapacity
		}
		l.log.Warn("launch failed", slog.String("party_id", partyID), slog.String("reason", reason), slog.Any("error", err))
		_, notices, done, ferr := l.store.FailLaunch(ctx, partyID, reason)
		if ferr != nil {
			// The party stays launching; the sweep tries again.
			l.log.Error("launch failure not recorded", slog.String("party_id", partyID), slog.Any("error", ferr))
			return ReasonAllocatorUnavailable
		}
		if !done {
			l.results.WithLabelValues("superseded").Inc()
			return "superseded"
		}
		l.publish(notices)
		l.results.WithLabelValues(reason).Inc()
		return reason
	}

	p, notices, done, err := l.store.FinishLaunch(ctx, partyID, a.AllocationID, a.Address, a.Port)
	if err != nil {
		l.log.Error("granted launch not recorded", slog.String("party_id", partyID), slog.Any("error", err))
		return ReasonAllocatorUnavailable
	}
	if !done {
		l.results.WithLabelValues("superseded").Inc()
		return "superseded"
	}

	// Each member gets their own party.launching carrying only their own ticket
	// (doc 14 §3, §7).
	for _, m := range p.Members {
		ticket, ok := a.Tickets[m.PlayerID]
		if !ok {
			l.log.Warn("no ticket for a member", slog.String("party_id", partyID), slog.String("player_id", m.PlayerID))
			continue
		}
		notices = append(notices, store.Notice{PlayerID: m.PlayerID, Type: events.TypePartyLaunching,
			Payload: map[string]any{
				"party_id":          p.ID,
				"revision":          p.Revision,
				"allocation_id":     a.AllocationID,
				"address":           a.Address,
				"port":              a.Port,
				"ticket":            ticket,
				"ticket_expires_at": allocator.TicketExpiry(ticket),
			}})
	}
	l.publish(notices)
	l.results.WithLabelValues("in_game").Inc()
	return "in_game"
}

// Sweep repeats the launch of every party stuck in launching (doc 14 §6). The Allocator
// is idempotent per party, so this cannot reserve a second server.
func (l *Launcher) Sweep(ctx context.Context) {
	stuck, err := l.store.StuckLaunching(ctx, StuckAfter)
	if err != nil {
		l.log.Warn("stuck launch sweep failed", slog.Any("error", err))
		return
	}
	for _, s := range stuck {
		l.log.Info("repeating a stuck launch", slog.String("party_id", s.PartyID))
		l.Launch(ctx, s.PartyID, s.Members)
	}
}

// Run sweeps every SweepInterval until ctx ends, then waits for background launches.
func (l *Launcher) Run(ctx context.Context) {
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.Wait()
			return
		case <-t.C:
			l.Sweep(ctx)
		}
	}
}

func (l *Launcher) publish(notices []store.Notice) { publish(l.ctx, l.pub, l.log, notices) }

// publish sends notices on a context of its own: the outcome is committed, so the
// members are told even while the process is shutting down.
func publish(parent context.Context, pub Publisher, log *slog.Logger, notices []store.Notice) {
	if pub == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	for _, n := range notices {
		if _, err := pub.Publish(ctx, n.PlayerID, n.Type, n.Payload); err != nil {
			log.Warn("event not published", slog.String("type", n.Type), slog.Any("error", err))
		}
	}
}
