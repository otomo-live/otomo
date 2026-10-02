package ratelimit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
	"uuid"
)

func testLimiter(t *testing.T) *Limiter {
	t.Helper()
	url := os.Getenv("SESSION_TEST_VALKEY_URL")
	if url == "" {
		t.Skip("SESSION_TEST_VALKEY_URL is not set; skipping the tests that need Valkey")
	}
	opt, err := valkey.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	opt.DisableCache = true
	vk, err := valkey.NewClient(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vk.Close)
	return New(vk)
}

// TestTheLimitHoldsForTheWindow is SES-C5: the third action of a limit of two is refused
// with the rest of the window to wait.
func TestTheLimitHoldsForTheWindow(t *testing.T) {
	l := testLimiter(t)
	player := uuid.NewV7().String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.vk.Do(ctx, l.vk.B().Del().Key("ratelimit:test:"+player).Build()).Error()
	})

	for i := range 2 {
		if wait, err := l.Allow(t.Context(), "test", player, 2, time.Minute); err != nil || wait != 0 {
			t.Fatalf("action %d = %v, %v; want allowed", i+1, wait, err)
		}
	}
	wait, err := l.Allow(t.Context(), "test", player, 2, time.Minute)
	if err != nil || wait <= 50*time.Second || wait > time.Minute {
		t.Errorf("third action = %v, %v; want refused with about a minute to wait", wait, err)
	}
	// Another player has their own window.
	if wait, err := l.Allow(t.Context(), "test", player+"-other", 2, time.Minute); err != nil || wait != 0 {
		t.Errorf("another player = %v, %v; want allowed", wait, err)
	}
	_ = l.vk.Do(t.Context(), l.vk.B().Del().Key("ratelimit:test:"+player+"-other").Build()).Error()
}
