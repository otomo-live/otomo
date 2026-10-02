package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool returns a pool against PATCH_TEST_DATABASE_URL, or skips when the variable
// is unset. Patch has no migrations of its own and deliberately takes no goose
// dependency, so the tests assume Config's migrations are applied and skip when the
// table the loader reads does not exist.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("PATCH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PATCH_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.channel_head') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check channel_head: %v", err)
	}
	if !exists {
		t.Skip("channel_head does not exist; apply Config's migrations to PATCH_TEST_DATABASE_URL")
	}
	return pool
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestLoadAllServesBootstrappedChannels checks the three seeded channels: each entry's
// Body must hash to the row's manifest_sha256 and its ETag must be that hash quoted.
func TestLoadAllServesBootstrappedChannels(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var holder Holder
	loader := &Loader{DB: pool, Log: quietLogger(), Holder: &holder}
	if err := loader.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	set := holder.Load()
	if set == nil {
		t.Fatal("LoadAll published no set")
	}
	for _, channel := range []string{"dev", "staging", "live"} {
		e, ok := set.Get(channel)
		if !ok {
			t.Errorf("%s: no entry loaded", channel)
			continue
		}

		sum := sha256.Sum256(e.Body)
		got := hex.EncodeToString(sum[:])

		var stored string
		if err := pool.QueryRow(ctx,
			`SELECT r.manifest_sha256
			   FROM release r JOIN channel_head h USING (release_id)
			  WHERE h.channel = $1`, channel).Scan(&stored); err != nil {
			t.Fatalf("%s: read manifest_sha256: %v", channel, err)
		}

		if got != stored {
			t.Errorf("%s: Body hashes to %s, row says %s", channel, got, stored)
		}
		if e.ETag != `"`+stored+`"` {
			t.Errorf("%s: ETag = %s, want %q", channel, e.ETag, `"`+stored+`"`)
		}
	}
}

// TestLoadAllRejectsTamperedHash points a channel at a scratch release whose
// manifest_sha256 is wrong and checks the loader drops it while still publishing the
// channels that did verify. Everything happens inside a transaction that is rolled
// back, so no committed data is modified.
func TestLoadAllRejectsTamperedHash(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		raw    string
		minVer string
	)
	if err := tx.QueryRow(ctx,
		`SELECT r.manifest::text, r.min_client_version
		   FROM release r JOIN channel_head h USING (release_id)
		  WHERE h.channel = 'live'`).Scan(&raw, &minVer); err != nil {
		t.Fatalf("read the live bootstrap release: %v", err)
	}

	var scratchID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO release (channel, manifest, manifest_sha256, min_client_version, message, created_by)
		 VALUES ('live', $1::jsonb, repeat('0', 64), $2, 'scratch', 'test')
		 RETURNING release_id`, raw, minVer).Scan(&scratchID); err != nil {
		t.Fatalf("insert scratch release: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE channel_head SET release_id = $1 WHERE channel = 'live'`, scratchID); err != nil {
		t.Fatalf("point live at the scratch release: %v", err)
	}

	var holder Holder
	loader := &Loader{DB: pool, Log: quietLogger(), Holder: &holder, q: tx}
	// dev and staging still verify, so this succeeds; the tampered live row is logged
	// and dropped rather than failing the whole load.
	if err := loader.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll with one tampered row: %v", err)
	}

	set := holder.Load()
	if _, ok := set.Get("live"); ok {
		t.Error("the tampered live release was published")
	}
	for _, channel := range []string{"dev", "staging"} {
		if _, ok := set.Get(channel); !ok {
			t.Errorf("the good %s row was dropped with the bad one", channel)
		}
	}
}

// A reload that finds a tampered head keeps serving the channel's last good entry: one
// bad release must not take a working channel offline.
func TestLoadAllKeepsLastGoodEntryForARejectedChannel(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var holder Holder
	if err := (&Loader{DB: pool, Log: quietLogger(), Holder: &holder}).LoadAll(ctx); err != nil {
		t.Fatalf("initial LoadAll: %v", err)
	}
	before, ok := holder.Load().Get("live")
	if !ok {
		t.Fatal("live not loaded initially")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var scratchID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO release (channel, manifest, manifest_sha256, min_client_version, message, created_by)
		 VALUES ('live', '{"tampered":true}'::jsonb, repeat('0', 64), '0.0.0', 'scratch', 'test')
		 RETURNING release_id`).Scan(&scratchID); err != nil {
		t.Fatalf("insert scratch release: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE channel_head SET release_id = $1 WHERE channel = 'live'`, scratchID); err != nil {
		t.Fatalf("point live at the scratch release: %v", err)
	}

	if err := (&Loader{DB: pool, Log: quietLogger(), Holder: &holder, q: tx}).LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll with a tampered live row: %v", err)
	}
	after, ok := holder.Load().Get("live")
	if !ok {
		t.Fatal("live vanished after a reload rejected its new row")
	}
	if after.ETag != before.ETag || after.ReleaseID != before.ReleaseID {
		t.Errorf("live = release %d %s, want the last good release %d %s",
			after.ReleaseID, after.ETag, before.ReleaseID, before.ETag)
	}
}
