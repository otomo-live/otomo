package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/otomo-live/otomo/services/patch/internal/config"
	"github.com/otomo-live/otomo/services/patch/internal/store"
)

// TestReady needs a real Postgres, because "the pool can be built against the patch_ro
// URL and answers a ping" is a property of the database rather than of this package.
// It skips when PATCH_TEST_DATABASE_URL is unset, so `go test ./...` stays runnable
// without one.
func TestReady(t *testing.T) {
	url := os.Getenv("PATCH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PATCH_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	db, err := store.NewPool(ctx, config.Config{DatabaseURL: url, DBMaxConns: 4})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
}
