package api

import "testing"

func TestTailLimiterPerUser(t *testing.T) {
	l := newTailLimiter(2, 10)

	r1, reason := l.acquire("staff-1")
	if reason != "" {
		t.Fatalf("first acquire refused: %q", reason)
	}
	r2, reason := l.acquire("staff-1")
	if reason != "" {
		t.Fatalf("second acquire refused: %q", reason)
	}
	if _, reason := l.acquire("staff-1"); reason != "user" {
		t.Fatalf("third acquire reason = %q, want user", reason)
	}
	// A different subject is unaffected by the first subject's cap.
	if r3, reason := l.acquire("staff-2"); reason != "" {
		t.Fatalf("other subject acquire refused: %q", reason)
	} else {
		r3()
	}

	r1()
	if _, reason := l.acquire("staff-1"); reason != "" {
		t.Fatalf("acquire after release refused: %q", reason)
	}
	r2()
}

func TestTailLimiterGlobal(t *testing.T) {
	l := newTailLimiter(2, 3)
	var releases []func()
	// Three slots: two for "a" (its per-user cap) and one for "b".
	for _, subject := range []string{"a", "a", "b"} {
		r, reason := l.acquire(subject)
		if reason != "" {
			t.Fatalf("acquire %s refused: %q", subject, reason)
		}
		releases = append(releases, r)
	}
	// A fourth subject's first slot would exceed the global cap.
	if _, reason := l.acquire("c"); reason != "global" {
		t.Fatalf("acquire past the global cap reason = %q, want global", reason)
	}
	for _, r := range releases {
		r()
	}
	// Releasing frees both per-user and global counts.
	r, reason := l.acquire("c")
	if reason != "" {
		t.Fatalf("acquire after all releases refused: %q", reason)
	}
	r()
}

func TestTailLimiterReleaseIsIdempotent(t *testing.T) {
	l := newTailLimiter(1, 2)
	release, reason := l.acquire("staff-1")
	if reason != "" {
		t.Fatalf("acquire refused: %q", reason)
	}
	release()
	release()

	if _, reason := l.acquire("staff-1"); reason != "" {
		t.Fatalf("acquire after a double release refused: %q", reason)
	}
	if _, reason := l.acquire("staff-2"); reason != "" {
		t.Fatalf("second subject refused after a double release: %q", reason)
	}
}

func TestTailLimiterDefaults(t *testing.T) {
	l := newTailLimiter(0, 0)
	if l.maxPerUser != defaultTailMaxPerUser || l.maxGlobal != defaultTailMaxGlobal {
		t.Fatalf("caps = %d/%d, want %d/%d", l.maxPerUser, l.maxGlobal, defaultTailMaxPerUser, defaultTailMaxGlobal)
	}
}
