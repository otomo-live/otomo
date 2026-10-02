package presence

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
	"uuid"
)

// testStore returns a Store on the Valkey SESSION_TEST_VALKEY_URL names, or skips the
// test when it is unset. Its clock is fixed and can be moved, so the rate limit, the
// trim and the count are tested without waiting.
func testStore(t *testing.T) (*Store, *time.Time) {
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

	now := time.Now()
	s := New(vk)
	s.now = func() time.Time { return now }
	return s, &now
}

// newPlayer returns a player id no other test uses, and removes its keys afterwards.
func newPlayer(t *testing.T, s *Store) string {
	t.Helper()
	p := uuid.NewV7().String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.vk.Do(ctx, s.vk.B().Del().Key(leaseKey(p)).Build()).Error()
		_ = s.vk.Do(ctx, s.vk.B().Zrem().Key(onlineKey).Member(p).Build()).Error()
	})
	return p
}

func TestHeartbeatWritesTheLeaseAndTheOnlineSet(t *testing.T) {
	s, now := testStore(t)
	p := newPlayer(t, s)

	beat, err := s.Heartbeat(t.Context(), p, StatusOnline)
	if err != nil || beat.RetryAfter != 0 || beat.Previous != StatusOffline {
		t.Fatalf("first beat = %+v, %v; want written, previously offline", beat, err)
	}
	lease, err := s.vk.Do(t.Context(), s.vk.B().Hgetall().Key(leaseKey(p)).Build()).AsStrMap()
	if err != nil || lease["status"] != StatusOnline || lease["updated_at"] == "" {
		t.Errorf("lease = %v, %v", lease, err)
	}
	ttl, err := s.vk.Do(t.Context(), s.vk.B().Pttl().Key(leaseKey(p)).Build()).AsInt64()
	if err != nil || ttl <= 55_000 || ttl > 60_000 {
		t.Errorf("lease TTL = %d ms, %v; want about 60 s", ttl, err)
	}
	score, err := s.vk.Do(t.Context(), s.vk.B().Zscore().Key(onlineKey).Member(p).Build()).AsInt64()
	if err != nil || score != now.UnixMilli() {
		t.Errorf("online score = %d, %v; want %d", score, err, now.UnixMilli())
	}
}

// TestAtMostOneBeatPer10Seconds is SES-B6: a beat 4 s after the last is refused with 6 s
// to wait, and one 10 s after is written.
func TestAtMostOneBeatPer10Seconds(t *testing.T) {
	s, now := testStore(t)
	p := newPlayer(t, s)
	if _, err := s.Heartbeat(t.Context(), p, StatusOnline); err != nil {
		t.Fatal(err)
	}

	*now = now.Add(4 * time.Second)
	beat, err := s.Heartbeat(t.Context(), p, StatusAway)
	if err != nil || beat.RetryAfter != 6*time.Second {
		t.Errorf("beat after 4 s = %+v, %v; want refused with 6 s to wait", beat, err)
	}
	if st, _ := s.Statuses(t.Context(), []string{p}); st[p] != StatusOnline {
		t.Errorf("a refused beat changed the status to %q", st[p])
	}

	*now = now.Add(6 * time.Second)
	beat, err = s.Heartbeat(t.Context(), p, StatusAway)
	if err != nil || beat.RetryAfter != 0 || beat.Previous != StatusOnline {
		t.Errorf("beat after 10 s = %+v, %v; want written, previously online", beat, err)
	}
}

// TestConcurrentBeatsWriteOnce: ten beats racing from one player, one gets through.
func TestConcurrentBeatsWriteOnce(t *testing.T) {
	s, _ := testStore(t)
	p := newPlayer(t, s)
	var (
		mu      sync.Mutex
		written int
		wg      sync.WaitGroup
	)
	for range 10 {
		wg.Go(func() {
			b, err := s.Heartbeat(t.Context(), p, StatusOnline)
			if err != nil {
				t.Error(err)
				return
			}
			if b.RetryAfter == 0 {
				mu.Lock()
				written++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if written != 1 {
		t.Errorf("%d beats written, want 1", written)
	}
}

func TestStatusesIsOfflineWithoutALease(t *testing.T) {
	s, _ := testStore(t)
	on, off := newPlayer(t, s), newPlayer(t, s)
	if _, err := s.Heartbeat(t.Context(), on, StatusInMenus); err != nil {
		t.Fatal(err)
	}
	got, err := s.Statuses(t.Context(), []string{on, off})
	if err != nil || got[on] != StatusInMenus || got[off] != StatusOffline {
		t.Errorf("Statuses = %v, %v", got, err)
	}
}

// TestAPlayerIsOffline60SecondsAfterTheLastBeat is SE-3's first criterion: the trim
// removes a player 60 s after their last beat, and the count no longer includes them.
// (The lease hash expires on Valkey's own clock; its TTL is checked above.)
func TestAPlayerIsOffline60SecondsAfterTheLastBeat(t *testing.T) {
	s, now := testStore(t)
	p := newPlayer(t, s)
	if _, err := s.Heartbeat(t.Context(), p, StatusOnline); err != nil {
		t.Fatal(err)
	}
	before, err := s.Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	*now = now.Add(59 * time.Second)
	gone, err := s.Trim(t.Context())
	if err != nil || contains(gone, p) {
		t.Errorf("trimmed at 59 s: %v, %v", gone, err)
	}

	*now = now.Add(2 * time.Second)
	after, err := s.Count(t.Context())
	if err != nil || after >= before {
		t.Errorf("count at 61 s = %d, %v; want fewer than the %d before", after, err, before)
	}
	gone, err = s.Trim(t.Context())
	if err != nil || !contains(gone, p) {
		t.Errorf("not trimmed at 61 s: %v, %v", gone, err)
	}
	// Trimmed once: a second trim does not return the player again.
	if again, _ := s.Trim(t.Context()); contains(again, p) {
		t.Error("a player was trimmed twice")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
