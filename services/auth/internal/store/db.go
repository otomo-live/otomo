// Package store is the service's Postgres access layer: the pool, its readiness
// probe, and the queries for signing_key, account, identity_binding and
// refresh_token.
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/otomo-live/otomo/services/auth/internal/config"
)

// ErrNotFound is wrapped into the error returned when a lookup matches no row, so
// callers can use errors.Is instead of importing pgx or matching on driver errors.
var ErrNotFound = errors.New("not found")

// readyProbeInterval is how long a readiness result is reused before Postgres is
// pinged again. Without it, a kubelet or load balancer polling /readyz every second
// would turn the readiness endpoint into a steady load on the database.
const readyProbeInterval = 10 * time.Second

// DB wraps the pgx pool and caches the most recent readiness probe so /readyz does
// not ping per request. The zero value is not usable; call NewPool. Safe for
// concurrent use.
type DB struct {
	Pool *pgxpool.Pool

	mu      sync.Mutex
	lastAt  time.Time
	lastErr error
}

// NewPool parses cfg.DatabaseURL, builds the pool and pings once, so a container
// with an unreachable or malformed database URL fails at startup rather than on its
// first request. On failure no pool is leaked: the connection is closed before the
// error is returned.
func NewPool(ctx context.Context, cfg config.Config) (*DB, error) {
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse AUTH_DATABASE_URL: %w", err)
	}
	pc.MaxConns = cfg.DBMaxConns

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &DB{Pool: pool, lastAt: time.Now()}, nil
}

// Close releases the pool. Callers should defer it immediately after a successful
// NewPool, so every later error path unwinds through it.
func (d *DB) Close() {
	d.Pool.Close()
}

// Ready reports whether Postgres is reachable, pinging at most once per
// readyProbeInterval and returning the cached verdict in between.
//
// The cached error is returned rather than a generic one, so /readyz tells an
// operator why the service is not ready. The first call always pings, since lastAt is
// set by NewPool.
func (d *DB) Ready(ctx context.Context) error {
	d.mu.Lock()
	if time.Since(d.lastAt) < readyProbeInterval {
		err := d.lastErr
		d.mu.Unlock()
		return err
	}
	d.mu.Unlock()

	err := d.Pool.Ping(ctx)

	d.mu.Lock()
	d.lastAt = time.Now()
	d.lastErr = err
	d.mu.Unlock()

	if err != nil {
		return fmt.Errorf("postgres unreachable: %w", err)
	}
	return nil
}
