package launch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/otomo-live/otomo/services/session/internal/allocator"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

// Doc 14 §6's missed-callback repair: every 30 s, parties in_game for more than 30 s.
const (
	RepairInterval = 30 * time.Second
	RepairAfter    = 30 * time.Second
)

// ReasonEnded is the Allocator's reason for a match the game server reported over, and
// the reason Session uses when the Allocator gives none (doc 14 §4.4).
const ReasonEnded = "ended"

// Where a return came from, for session_return_total.
const (
	SourceCallback = "callback"
	SourceRepair   = "repair"
)

// ReturnStore is what the Returner needs from the party store. *store.DB implements it.
type ReturnStore interface {
	ReturnFromGame(ctx context.Context, partyID, allocationID, reason string) (*store.Party, []store.Notice, bool, error)
	InGameLongerThan(ctx context.Context, olderThan time.Duration) ([]store.InGame, error)
}

// StatusReader reads an allocation back from the Allocator. *allocator.Client implements
// it.
type StatusReader interface {
	Get(ctx context.Context, allocationID string) (*allocator.Status, error)
}

// Returner brings a lobby back from its match (LB-4): when the Allocator's
// callback says the allocation ended, and when the repair poll finds that it did and the
// callback never came. Build it with NewReturner.
type Returner struct {
	ctx    context.Context
	store  ReturnStore
	status StatusReader // nil: the repair poll does nothing
	pub    Publisher
	log    *slog.Logger

	returns *prometheus.CounterVec
}

// NewReturner returns a Returner. status may be nil when Session has no Allocator key:
// callbacks still return parties, and the repair poll is off. Events are published on a
// context derived from ctx that outlives its cancellation, as the Launcher's are.
func NewReturner(ctx context.Context, s ReturnStore, status StatusReader, p Publisher, log *slog.Logger) *Returner {
	if log == nil {
		log = slog.Default()
	}
	r := &Returner{
		ctx: ctx, store: s, status: status, pub: p, log: log,
		returns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "session_return_total",
			Help: "Lobbies returned from a match to forming, by what noticed the match ended (callback, repair).",
		}, []string{"source"}),
	}
	for _, s := range []string{SourceCallback, SourceRepair} {
		r.returns.WithLabelValues(s)
	}
	return r
}

// Collectors returns the Returner's metrics, for the service's registry.
func (r *Returner) Collectors() []prometheus.Collector {
	return []prometheus.Collector{r.returns}
}

// Return puts partyID back into forming if it is still in_game on allocationID, and
// tells its members. It reports whether the party changed; false with a nil error means
// the party is gone or already elsewhere, which is not a failure (doc 14 §4.4). An empty
// reason is ReasonEnded.
func (r *Returner) Return(ctx context.Context, partyID, allocationID, reason, source string) (bool, error) {
	if reason == "" {
		reason = ReasonEnded
	}
	_, notices, done, err := r.store.ReturnFromGame(ctx, partyID, allocationID, reason)
	if err != nil || !done {
		return false, err
	}
	r.log.Info("party returned from its match",
		slog.String("party_id", partyID), slog.String("allocation_id", allocationID),
		slog.String("reason", reason), slog.String("source", source))
	publish(r.ctx, r.pub, r.log, notices)
	r.returns.WithLabelValues(source).Inc()
	return true, nil
}

// Repair asks the Allocator about every party in_game for longer than RepairAfter and
// returns each whose allocation is no longer live, with the allocation's end_reason
// (doc 14 §6). An allocation the Allocator does not know is not hosting anything, so its
// party is returned too. Any other error ends the round: the Allocator is probably
// unreachable, and the next round asks again.
func (r *Returner) Repair(ctx context.Context) {
	if r.status == nil {
		return
	}
	parties, err := r.store.InGameLongerThan(ctx, RepairAfter)
	if err != nil {
		r.log.Warn("missed-callback repair failed", slog.Any("error", err))
		return
	}
	for _, g := range parties {
		st, err := r.status.Get(ctx, g.AllocationID)
		var reason string
		switch {
		case errors.Is(err, allocator.ErrNotFound):
			reason = ReasonEnded
		case err != nil:
			r.log.Warn("missed-callback repair stopped: the Allocator did not answer",
				slog.String("allocation_id", g.AllocationID), slog.Any("error", err))
			return
		case st.Live():
			continue
		default:
			reason = st.EndReason
		}
		if _, err := r.Return(ctx, g.PartyID, g.AllocationID, reason, SourceRepair); err != nil {
			r.log.Warn("a party whose match ended was not returned",
				slog.String("party_id", g.PartyID), slog.Any("error", err))
		}
	}
}

// Run repairs every RepairInterval until ctx ends.
func (r *Returner) Run(ctx context.Context) {
	t := time.NewTicker(RepairInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Repair(ctx)
		}
	}
}
