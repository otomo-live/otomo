package presence

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

// Backend is the presence store. *Store implements it; tests supply a fake.
type Backend interface {
	Heartbeat(ctx context.Context, player, status string) (Beat, error)
	Statuses(ctx context.Context, players []string) (map[string]string, error)
	Trim(ctx context.Context) ([]string, error)
	Count(ctx context.Context) (int64, error)
}

// Friends lists a player's accepted friends. *store.DB implements it.
type Friends interface {
	AcceptedFriends(ctx context.Context, player string) ([]string, error)
}

// Publisher sends one event to a player's stream. *events.Client implements it.
type Publisher interface {
	Publish(ctx context.Context, playerID, typ string, payload any) (events.Event, error)
}

// Service is presence as the handlers and the trim loop use it: a heartbeat that tells
// online friends when the status changes (SES-B3), a loop that trims the online set,
// tells friends who went offline (SES-B4) and sets the online gauge (SES-B5).
type Service struct {
	backend Backend
	friends Friends // nil: nobody is told
	pub     Publisher
	log     *slog.Logger

	online prometheus.Gauge
}

// NewService returns a Service. friends and pub may be nil, and then status changes are
// not announced.
func NewService(b Backend, friends Friends, pub Publisher, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		backend: b, friends: friends, pub: pub, log: log,
		// Named otomo_ rather than session_ on purpose: the Dashboard overview sums
		// otomo_online_players across services (design/13-player-plane-plan.md SE-3).
		online: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "otomo_online_players",
			Help: "Players whose last presence heartbeat is under 60 s old, refreshed every 15 s.",
		}),
	}
}

// Collectors returns the Service's metrics, for the service's registry.
func (s *Service) Collectors() []prometheus.Collector {
	return []prometheus.Collector{s.online}
}

// Heartbeat records one beat from player. When it changes their status, their online
// friends get presence.changed. A refused beat (Beat.RetryAfter above zero) tells nobody.
func (s *Service) Heartbeat(ctx context.Context, player, status string) (Beat, error) {
	b, err := s.backend.Heartbeat(ctx, player, status)
	if err != nil {
		return Beat{}, err
	}
	if b.Changed(status) {
		s.announce(ctx, player, status)
	}
	return b, nil
}

// Statuses returns each player's status, StatusOffline without a lease.
func (s *Service) Statuses(ctx context.Context, players []string) (map[string]string, error) {
	return s.backend.Statuses(ctx, players)
}

// Tick trims the online set, tells the friends of each player who went offline, and
// sets the online gauge.
func (s *Service) Tick(ctx context.Context) {
	gone, err := s.backend.Trim(ctx)
	if err != nil {
		s.log.Warn("presence trim failed", slog.Any("error", err))
	}
	for _, player := range gone {
		s.announce(ctx, player, StatusOffline)
	}
	n, err := s.backend.Count(ctx)
	if err != nil {
		s.log.Warn("online player count failed", slog.Any("error", err))
		return
	}
	s.online.Set(float64(n))
}

// Run ticks every TrimInterval until ctx ends, starting at once so the gauge is set
// from the first scrape.
func (s *Service) Run(ctx context.Context) {
	s.Tick(ctx)
	t := time.NewTicker(TrimInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// announce sends presence.changed{player_id, status} to each of player's accepted
// friends who is online now (doc 04 §4: friends only). A failure is logged and not
// returned: events are hints, and the beat itself has been recorded.
func (s *Service) announce(ctx context.Context, player, status string) {
	if s.friends == nil || s.pub == nil {
		return
	}
	friends, err := s.friends.AcceptedFriends(ctx, player)
	if err != nil {
		s.log.Warn("presence change not announced: friends unreadable", slog.Any("error", err))
		return
	}
	if len(friends) == 0 {
		return
	}
	statuses, err := s.backend.Statuses(ctx, friends)
	if err != nil {
		s.log.Warn("presence change not announced: friend presence unreadable", slog.Any("error", err))
		return
	}
	payload := map[string]any{"player_id": player, "status": status}
	for _, f := range friends {
		if statuses[f] == StatusOffline {
			continue
		}
		if _, err := s.pub.Publish(ctx, f, events.TypePresenceChange, payload); err != nil {
			s.log.Warn("event not published", slog.String("type", events.TypePresenceChange), slog.Any("error", err))
		}
	}
}
