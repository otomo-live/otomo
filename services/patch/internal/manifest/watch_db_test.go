package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// devReleaseID reads the release dev currently points at, so a test can restore it.
func devReleaseID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()

	var id int64
	if err := pool.QueryRow(ctx,
		`SELECT release_id FROM channel_head WHERE channel = 'dev'`).Scan(&id); err != nil {
		t.Fatalf("read dev head: %v", err)
	}
	return id
}

// restoreDev records the original dev release and restores it after the test, then
// notifies dev so a listener left behind by the test is not stale.
func restoreDev(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) {
	t.Helper()

	t.Cleanup(func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if _, err := pool.Exec(rctx,
			`UPDATE channel_head SET release_id = $1, updated_by = 'watch-test', updated_at = now()
			  WHERE channel = 'dev'`, id); err != nil {
			t.Errorf("restore dev head: %v", err)
			return
		}
		if _, err := pool.Exec(rctx, `SELECT pg_notify($1, 'dev')`, notifyChannel); err != nil {
			t.Errorf("notify dev restore: %v", err)
		}
	})
}

// newDevRelease inserts one dev release with a canonical body and its real sha256,
// built exactly the way migration 00002 builds the bootstrap release. It commits, so
// the watcher's separate connection can see it.
func newDevRelease(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()

	var id int64
	if err := pool.QueryRow(ctx,
		`SELECT nextval(pg_get_serial_sequence('release', 'release_id'))`).Scan(&id); err != nil {
		t.Fatalf("allocate release id: %v", err)
	}

	body := fmt.Sprintf(
		`{"channel":"dev","config":{},"format":1,"min_client_version":"0.0.0","packs":[],"release_id":%d}`,
		id)
	canon, err := Canonical([]byte(body))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	sum := sha256.Sum256(canon)

	if _, err := pool.Exec(ctx,
		`INSERT INTO release (release_id, channel, manifest, manifest_sha256, min_client_version, message, created_by)
		 VALUES ($1, 'dev', $2::jsonb, $3, '0.0.0', 'watch test', 'watch-test')`,
		id, string(canon), hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("insert release: %v", err)
	}
	return id
}

// pointDev commits a channel_head move, optionally in the same transaction as the
// pg_notify Config issues, which is what makes delivery conditional on commit.
func pointDev(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, notify bool) {
	t.Helper()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE channel_head SET release_id = $1, updated_by = 'watch-test', updated_at = now()
		  WHERE channel = 'dev'`, id); err != nil {
		t.Fatalf("point dev at %d: %v", id, err)
	}
	if notify {
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, 'dev')`, notifyChannel); err != nil {
			t.Fatalf("pg_notify: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// reloadMetrics mirrors the patch_manifest_reload_total counter the server registers,
// so a DB-backed test can assert which source caused a reload.
func reloadMetrics() *prometheus.CounterVec {
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patch_manifest_reload_total",
		Help: "Manifest reloads published, by source (notify, poll or reconnect).",
	}, []string{"source"})
	reg.MustRegister(c)
	return c
}

// waitFor polls a condition until it holds or the deadline passes. It never blocks
// longer than timeout and never sleeps more than 20 ms.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// TestWatcherReloadsOnNotify is the happy path: a committed publish that also notifies
// must reach the holder's dev entry without waiting for any poll.
func TestWatcherReloadsOnNotify(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	original := devReleaseID(t, ctx, pool)
	restoreDev(t, ctx, pool, original)

	holder := &Holder{}
	loader := &Loader{DB: pool, Log: quietLogger(), Holder: holder}
	if err := loader.LoadAll(ctx); err != nil {
		t.Fatalf("initial LoadAll: %v", err)
	}

	reloads := reloadMetrics()
	watcher := &Watcher{
		Loader:       loader,
		Pool:         pool,
		Log:          quietLogger(),
		PollInterval: time.Hour, // keep the poll out of the notify path
		OnReload: func(source string) {
			reloads.WithLabelValues(source).Inc()
		},
	}

	go watcher.Run(ctx)
	waitFor(t, 5*time.Second, watcher.isListening, "listener to connect")

	newID := newDevRelease(t, ctx, pool)
	pointDev(t, ctx, pool, newID, true)

	waitFor(t, 5*time.Second, func() bool {
		e, ok := holder.Load().Get("dev")
		return ok && e.ReleaseID == newID
	}, "dev to reload via notify")

	if got := testutil.ToFloat64(reloads.WithLabelValues("notify")); got < 1 {
		t.Errorf("patch_manifest_reload_total{source=notify} = %v, want >= 1", got)
	}
}

// TestWatcherFallsBackToPoll kills the listener and checks that a change committed
// without a notification is still picked up by the poll within its interval.
func TestWatcherFallsBackToPoll(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	original := devReleaseID(t, ctx, pool)
	restoreDev(t, ctx, pool, original)

	holder := &Holder{}
	loader := &Loader{DB: pool, Log: quietLogger(), Holder: holder}
	if err := loader.LoadAll(ctx); err != nil {
		t.Fatalf("initial LoadAll: %v", err)
	}

	reloads := reloadMetrics()
	watcher := &Watcher{
		Loader:           loader,
		Pool:             pool,
		Log:              quietLogger(),
		PollInterval:     200 * time.Millisecond,
		listenerDisabled: true, // exercise the poll in isolation
		OnReload: func(source string) {
			reloads.WithLabelValues(source).Inc()
		},
	}

	go watcher.Run(ctx)

	newID := newDevRelease(t, ctx, pool)
	pointDev(t, ctx, pool, newID, false)

	waitFor(t, 2*time.Second, func() bool {
		e, ok := holder.Load().Get("dev")
		return ok && e.ReleaseID == newID
	}, "dev to reload via poll")

	if got := testutil.ToFloat64(reloads.WithLabelValues("poll")); got < 1 {
		t.Errorf("patch_manifest_reload_total{source=poll} = %v, want >= 1", got)
	}
}

// TestWatcherReconnectsAndReloadsAll terminates the listener backend, checks readiness
// only turns false after the poll interval, and checks that a change committed while
// the listener was down is loaded by the reconnect's LoadAll.
func TestWatcherReconnectsAndReloadsAll(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	original := devReleaseID(t, ctx, pool)
	restoreDev(t, ctx, pool, original)

	holder := &Holder{}
	loader := &Loader{DB: pool, Log: quietLogger(), Holder: holder}
	if err := loader.LoadAll(ctx); err != nil {
		t.Fatalf("initial LoadAll: %v", err)
	}

	reloads := reloadMetrics()
	watcher := &Watcher{
		Loader:       loader,
		Pool:         pool,
		Log:          quietLogger(),
		PollInterval: 200 * time.Millisecond,
		pollDisabled: true, // prove the reconnect's LoadAll does the work
		OnReload: func(source string) {
			reloads.WithLabelValues(source).Inc()
		},
	}

	go watcher.Run(ctx)
	waitFor(t, 5*time.Second, watcher.isListening, "listener to connect")

	pid := watcher.listenerPID()
	if pid == 0 {
		t.Fatal("listener reports no backend pid")
	}
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate listener %d: %v", pid, err)
	}

	// Immediately after the drop the outage is younger than the poll interval, so
	// readiness must still pass.
	if err := watcher.Ready(); err != nil {
		t.Errorf("Ready immediately after the drop = %v, want nil", err)
	}
	waitFor(t, 5*time.Second, func() bool { return watcher.Ready() != nil },
		"readiness to report a disconnected listener")

	// Commit a change while the listener is down and off. Poll is disabled, so only
	// the reconnect's LoadAll can pick it up.
	newID := newDevRelease(t, ctx, pool)
	pointDev(t, ctx, pool, newID, false)

	waitFor(t, 10*time.Second, func() bool {
		if !watcher.isListening() {
			return false
		}
		e, ok := holder.Load().Get("dev")
		return ok && e.ReleaseID == newID
	}, "reconnect to reload all channels")

	if got := testutil.ToFloat64(reloads.WithLabelValues("reconnect")); got < 1 {
		t.Errorf("patch_manifest_reload_total{source=reconnect} = %v, want >= 1", got)
	}
}

// TestWatcherReadyDrivesStateDirectly exercises Ready's boundary without touching
// Postgres: a listener that never connected counts as down from Run's start, a fresh
// disconnect is not an outage until the poll interval has passed, and a connected
// listener is ready regardless of how long it was down before.
func TestWatcherReadyDrivesStateDirectly(t *testing.T) {
	base := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	now := base
	w := &Watcher{
		PollInterval: 20 * time.Second,
		now:          func() time.Time { return now },
	}

	if err := w.Ready(); err != nil {
		t.Fatalf("Ready before Run = %v, want nil", err)
	}

	// Run started but no connection has ever been established.
	w.mu.Lock()
	w.running = true
	w.downSince = base
	w.mu.Unlock()

	if err := w.Ready(); err != nil {
		t.Fatalf("Ready within the poll interval = %v, want nil", err)
	}
	now = base.Add(21 * time.Second)
	err := w.Ready()
	if err == nil || !strings.Contains(err.Error(), "release listener disconnected for") {
		t.Fatalf("Ready after the poll interval = %v, want a disconnected error", err)
	}

	// A connected listener is ready even though it was down a moment ago.
	w.mu.Lock()
	w.listening = true
	w.mu.Unlock()
	if err := w.Ready(); err != nil {
		t.Fatalf("Ready while connected = %v, want nil", err)
	}

	// A fresh disconnect starts a new outage window.
	w.mu.Lock()
	w.listening = false
	w.downSince = now
	w.mu.Unlock()
	now = now.Add(20 * time.Second)
	if err := w.Ready(); err != nil {
		t.Fatalf("Ready exactly at the poll interval = %v, want nil", err)
	}
	now = now.Add(time.Second)
	if w.Ready() == nil {
		t.Fatal("Ready after the new outage window passed = nil, want a disconnected error")
	}
}
