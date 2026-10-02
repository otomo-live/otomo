// Package directory keeps the proxy's view of which game servers exist.
//
// The Allocator owns the game-server registry and publishes it at
// GET {allocator}/internal/servers with the proxy's service key. The proxy polls it on
// a timer and keeps the last good snapshot: a failed poll is an Allocator blip, and
// routing from a slightly stale table is better than dropping every new session. When a
// ticket names a server the snapshot does not know, the proxy forces one extra refresh,
// rate-limited to once per second, so a server that registered moments ago is found
// without waiting out the poll interval.
//
// The snapshot is read on the UDP hot path, so Lookup must be cheap: it takes an
// RWMutex read lock and a map lookup, nothing more.
package directory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/metrics"
)

// maxDirectoryBytes bounds the directory response the proxy will read, for the same
// reason the JWKS fetch is bounded: the Allocator is trusted, but a broken deployment
// must not be able to exhaust proxy memory.
const maxDirectoryBytes = 1 << 20

// Server is one entry in the Allocator's directory. InternalAddr is the Docker-network
// address the proxy dials; State is kept for completeness even though routing only
// cares that the server is present, because the Allocator is the authority on whether
// a server should be used.
type Server struct {
	ServerID     string `json:"server_id"`
	InternalAddr string `json:"internal_addr"`
	State        string `json:"state"`
}

// Config is everything the Directory needs. Only URL is strictly required; the rest
// exist so tests can inject a clock and a client, and so a missing Logger or Metrics
// falls back to a working default.
type Config struct {
	// URL is the full endpoint, including the /internal/servers path. Key is the
	// base64url service key presented as a bearer token.
	URL string
	Key string

	// Refresh is the poll period. Now is the clock used for the once-per-second guard;
	// nil means time.Now. Client is the HTTP client; nil means a 5 s-timeout client.
	Refresh time.Duration
	Now     func() time.Time
	Client  *http.Client

	Metrics *metrics.Metrics
	Logger  *slog.Logger
}

// Directory is the polled server table. Build it with New and run it with Run, or call
// Refresh directly to load it synchronously.
type Directory struct {
	cfg    Config
	client *http.Client
	now    func() time.Time
	log    *slog.Logger
	m      *metrics.Metrics

	// gate serialises every refresh and guards lastAttempt. It is what makes the
	// once-per-second rule exact even when several handshakes miss the same server at
	// the same moment.
	gate        sync.Mutex
	lastAttempt time.Time

	// mu guards the snapshot the UDP read loop reads.
	mu      sync.RWMutex
	servers map[string]Server
	loaded  bool
}

// New returns an empty Directory. Nothing is fetched until Refresh or Run is called, so
// construction cannot block start-up on an Allocator that is not up yet.
func New(cfg Config) *Directory {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Directory{
		cfg:     cfg,
		client:  client,
		now:     cfg.Now,
		log:     log,
		m:       cfg.Metrics,
		servers: make(map[string]Server),
	}
}

// Run polls until ctx is cancelled, starting with one immediate refresh so readiness
// does not wait a full period. Poll failures are logged and counted and leave the
// previous snapshot in place.
func (d *Directory) Run(ctx context.Context) {
	if err := d.Refresh(ctx); err != nil {
		d.log.Warn("initial server directory refresh failed", slog.Any("error", err))
	}

	ticker := time.NewTicker(d.cfg.Refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.Refresh(ctx); err != nil {
				d.log.Warn("server directory refresh failed", slog.Any("error", err))
			}
		}
	}
}

// Refresh fetches the directory and, on success, replaces the snapshot atomically. A
// failure leaves the last good snapshot untouched and returns the error. Every attempt,
// successful or not, advances the rate-limit clock so a broken Allocator cannot be
// hammered by handshakes missing a server.
func (d *Directory) Refresh(ctx context.Context) error {
	d.gate.Lock()
	defer d.gate.Unlock()
	return d.refreshLocked(ctx)
}

// RefreshIfStale refreshes only when the last attempt is at least min ago, and reports
// whether it refreshed. This is the proxy's path when a ticket names an unknown server:
// it is worth one immediate ask, but a stream of bad tickets must not turn into a
// request flood.
func (d *Directory) RefreshIfStale(ctx context.Context, min time.Duration) bool {
	d.gate.Lock()
	defer d.gate.Unlock()

	if !d.lastAttempt.IsZero() && d.now().Sub(d.lastAttempt) < min {
		return false
	}
	return d.refreshLocked(ctx) == nil
}

// refreshLocked performs the fetch. The caller holds gate, which is what makes
// lastAttempt and the in-flight request single-threaded.
func (d *Directory) refreshLocked(ctx context.Context) error {
	d.lastAttempt = d.now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.cfg.URL, nil)
	if err != nil {
		d.m.RecordDirectoryRefresh(metrics.RefreshError)
		return fmt.Errorf("build directory request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+d.cfg.Key)

	resp, err := d.client.Do(req)
	if err != nil {
		d.m.RecordDirectoryRefresh(metrics.RefreshError)
		return fmt.Errorf("fetch server directory: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		d.m.RecordDirectoryRefresh(metrics.RefreshError)
		return fmt.Errorf("fetch server directory: unexpected status %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDirectoryBytes))
	if err != nil {
		d.m.RecordDirectoryRefresh(metrics.RefreshError)
		return fmt.Errorf("read server directory: %w", err)
	}

	var body struct {
		Servers []Server `json:"servers"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		d.m.RecordDirectoryRefresh(metrics.RefreshError)
		return fmt.Errorf("decode server directory: %w", err)
	}

	table := make(map[string]Server, len(body.Servers))
	for _, s := range body.Servers {
		if s.ServerID == "" {
			// A nameless entry can never be looked up and would only hide a broken
			// Allocator response, so it is a decode error rather than a silent skip.
			d.m.RecordDirectoryRefresh(metrics.RefreshError)
			return fmt.Errorf("server directory entry has no server_id")
		}
		table[s.ServerID] = s
	}

	d.mu.Lock()
	d.servers = table
	d.loaded = true
	d.mu.Unlock()

	d.m.RecordDirectoryRefresh(metrics.RefreshOK)
	d.m.SetDirectoryServers(len(table))
	return nil
}

// Lookup returns the server named by id from the last good snapshot. The read lock is
// the only synchronisation on the UDP hot path.
func (d *Directory) Lookup(id string) (Server, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.servers[id]
	return s, ok
}

// Loaded reports whether a snapshot has ever been installed. A later failed refresh
// does not clear it, which is what keeps /readyz from flapping during an Allocator
// restart.
func (d *Directory) Loaded() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.loaded
}

// Len returns the size of the current snapshot. It exists for tests and log lines, not
// for routing.
func (d *Directory) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.servers)
}
