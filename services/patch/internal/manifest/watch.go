package manifest

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// notifyChannel is the Postgres channel Config publishes to. It sends the channel
// name as the payload inside the publish/rollback transaction, so the message is
// delivered only after that transaction commits.
const notifyChannel = "config_release"

const (
	// reconnectBackoff is the first delay after a listener failure; it doubles up to
	// reconnectMaxBackoff and resets once a connection is established.
	reconnectBackoff    = time.Second
	reconnectMaxBackoff = 30 * time.Second

	// defaultPollInterval is the fallback cadence design/03-patch-minimal.md §3 names:
	// 60 seconds, matching the client's own check interval.
	defaultPollInterval = 60 * time.Second
)

// Watcher keeps Patch's in-memory manifests fresh. It runs two independent sources of
// change: a LISTEN/NOTIFY listener that reacts immediately to a Config publish, and a
// fallback poll that re-reads channel_head in case a notification was missed while the
// listener was down. Build it with Loader and Pool set, then call Run.
//
// The listener holds a dedicated connection for its whole life: the connection is
// acquired from the pool and immediately hijacked, so the pool can never hand a
// connection that is stuck inside WaitForNotification to a query.
type Watcher struct {
	Loader       *Loader
	Pool         *pgxpool.Pool
	Log          *slog.Logger
	PollInterval time.Duration

	// OnReload, when set, is called once per successful reload with the source that
	// caused it: "notify", "poll" or "reconnect". It is how main feeds the
	// patch_manifest_reload_total counter in internal/server without this package
	// importing the server.
	OnReload func(source string)

	// now is injectable so readiness can be tested without sleeping. It is only read
	// and written by tests.
	now func() time.Time

	// Test hooks. listenerDisabled and pollDisabled let a test exercise one path in
	// isolation while Run still owns the other's lifecycle.
	listenerDisabled bool
	pollDisabled     bool

	mu        sync.Mutex
	running   bool
	listening bool
	downSince time.Time
	pid       uint32
}

// Run blocks until ctx is cancelled, running the listener and the fallback poll
// concurrently. The first LoadAll is performed here as part of the initial connect, so
// a caller that wants the manifests loaded before serving should still call
// Loader.LoadAll itself; Run will load them again without harm.
func (w *Watcher) Run(ctx context.Context) {
	w.mu.Lock()
	w.running = true
	w.listening = false
	w.downSince = w.timeNow()
	w.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w.listen(ctx)
	}()
	go func() {
		defer wg.Done()
		w.poll(ctx)
	}()
	wg.Wait()
}

// listen owns the dedicated LISTEN connection. It acquires, hijacks and listsens,
// loads every channel right after each successful connect (a notification may have
// been missed while the old connection was down), then blocks on notifications. Any
// error closes the connection, logs, backs off and reconnects.
func (w *Watcher) listen(ctx context.Context) {
	if w.listenerDisabled {
		return
	}

	backoff := reconnectBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		conn, err := w.Pool.Acquire(ctx)
		if err != nil {
			w.logger().Error("acquire listener connection failed", slog.Any("error", err))
			if !w.sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		pgConn := conn.Hijack()

		if _, err := pgConn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
			w.logger().Error("LISTEN failed; reconnecting", slog.Any("error", err))
			_ = pgConn.Close(context.WithoutCancel(ctx))
			if !w.sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		// A successful connect resets the backoff: a listener that flaps every few
		// minutes should reconnect after one second each time, not climb to 30.
		backoff = reconnectBackoff
		w.setListening(true, pgConn.PgConn().PID())
		w.logger().Info("listening for config releases",
			slog.String("channel", notifyChannel),
			slog.Uint64("backend_pid", uint64(pgConn.PgConn().PID())))

		if err := w.loadAll(ctx, "reconnect"); err != nil {
			w.logger().Error("load after listener connect failed", slog.Any("error", err))
		}

		err = w.notifications(ctx, pgConn)
		w.setListening(false, 0)
		_ = pgConn.Close(context.WithoutCancel(ctx))

		if ctx.Err() != nil {
			return
		}
		w.logger().Warn("release listener disconnected; reconnecting",
			slog.Any("error", err),
			slog.Duration("backoff", backoff))
		if !w.sleep(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// notifications blocks on the LISTEN connection until a message arrives or the
// connection fails. A payload naming a channel reloads just that channel; any other
// payload is treated as a signal to reload everything, because the listener cannot
// know which channel a future Config format might name.
func (w *Watcher) notifications(ctx context.Context, conn *pgx.Conn) error {
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}

		switch n.Payload {
		case "dev", "staging", "live":
			w.reload(ctx, n.Payload, "notify")
		default:
			if err := w.loadAll(ctx, "notify"); err != nil {
				w.logger().Error("reload on notification failed",
					slog.String("payload", n.Payload), slog.Any("error", err))
			}
		}
	}
}

// poll re-reads every channel head on a ticker and reloads only the channels whose
// release_id has changed. It is the safety net for notifications lost while the
// listener was down.
func (w *Watcher) poll(ctx context.Context) {
	if w.pollDisabled {
		return
	}
	ticker := time.NewTicker(w.pollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce(ctx)
		}
	}
}

const selectChannelHeads = `SELECT channel, release_id FROM channel_head`

// pollOnce is one poll tick: a single query over every channel head, then a Reload
// per channel that differs from what the holder is serving. A channel the holder has
// never seen counts as differing and is loaded.
func (w *Watcher) pollOnce(ctx context.Context) {
	rows, err := w.Pool.Query(ctx, selectChannelHeads)
	if err != nil {
		w.logger().Error("poll channel heads failed", slog.Any("error", err))
		return
	}
	defer rows.Close()

	current := w.Loader.Holder.Load()
	for rows.Next() {
		var (
			channel   string
			releaseID int64
		)
		if err := rows.Scan(&channel, &releaseID); err != nil {
			w.logger().Error("scan channel head failed", slog.Any("error", err))
			return
		}
		if e, ok := current.Get(channel); ok && e.ReleaseID == releaseID {
			continue
		}
		w.reload(ctx, channel, "poll")
	}
	if err := rows.Err(); err != nil {
		w.logger().Error("poll channel heads failed", slog.Any("error", err))
	}
}

// reload loads one channel and records the source that caused it.
func (w *Watcher) reload(ctx context.Context, channel, source string) {
	if err := w.Loader.Reload(ctx, channel); err != nil {
		w.logger().Error("reload failed",
			slog.String("channel", channel),
			slog.String("source", source),
			slog.Any("error", err))
		return
	}
	w.reloaded(source)
}

// loadAll loads every channel and records the source that caused it.
func (w *Watcher) loadAll(ctx context.Context, source string) error {
	if err := w.Loader.LoadAll(ctx); err != nil {
		return err
	}
	w.reloaded(source)
	return nil
}

func (w *Watcher) reloaded(source string) {
	if w.OnReload == nil {
		return
	}
	w.OnReload(source)
}

// Ready reports whether the listener is healthy. It is nil before Run has started and
// while a connection is established; once the listener has been down for longer than
// the poll interval it returns the age of the outage, because at that point a client
// may have missed a notification by more than the documented fallback window. A
// listener that has never connected since Run started counts as down from that moment.
func (w *Watcher) Ready() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running || w.listening {
		return nil
	}
	down := w.timeNow().Sub(w.downSince)
	if down <= w.pollIntervalLocked() {
		return nil
	}
	return fmt.Errorf("release listener disconnected for %s", down.Round(time.Millisecond))
}

// pollInterval returns the configured fallback cadence, or the default when unset.
func (w *Watcher) pollInterval() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pollIntervalLocked()
}

func (w *Watcher) pollIntervalLocked() time.Duration {
	if w.PollInterval <= 0 {
		return defaultPollInterval
	}
	return w.PollInterval
}

func (w *Watcher) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

func (w *Watcher) timeNow() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *Watcher) setListening(up bool, pid uint32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.listening = up
	if up {
		w.pid = pid
		return
	}
	w.pid = 0
	w.downSince = w.timeNow()
}

// sleep waits for d or until ctx is done. It reports false when ctx ended.
func (w *Watcher) sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > reconnectMaxBackoff {
		return reconnectMaxBackoff
	}
	return d
}

// listenerPID returns the backend pid of the current LISTEN connection, or 0 when the
// listener is down. It exists so DB-backed tests can terminate exactly the listener.
func (w *Watcher) listenerPID() uint32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pid
}

// isListening reports whether a LISTEN connection is currently established. Tests use
// it to wait for the listener to come up before publishing a notification.
func (w *Watcher) isListening() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.listening
}
