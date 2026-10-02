package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/dashboard/internal/api"
	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// tailSource is a fake source.LogSource for the SSE integration tests: Tail emits whatever
// is put on lines until its context ends.
type tailSource struct {
	lines chan source.LogLine
}

func (s *tailSource) Query(context.Context, source.LogQuery) ([]source.LogLine, error) {
	return nil, nil
}

func (s *tailSource) Tail(ctx context.Context, _ source.LogQuery, out chan<- source.LogLine) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case line := <-s.lines:
			select {
			case out <- line:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// tailHandlers wires the SSE handler with a fake source and the given keep-alive interval.
func tailHandlers(src *tailSource, keepAlive time.Duration) *api.Handlers {
	return &api.Handlers{
		Logs: src,
		LogServices: func(context.Context) (promql.ServiceSet, error) {
			return promql.NewServiceSet("gateway"), nil
		},
		TailKeepAlive: keepAlive,
	}
}

// openTail opens one live tail and returns it with a cancel func that closes the client
// side. The plain client deliberately has no timeout: the harness's 5 s client would abort
// a stream these tests need to keep open.
func openTail(t *testing.T, h *harness, token string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+h.public+"/api/admin/dashboard/logs/tail", nil)
	if err != nil {
		cancel()
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open tail: %v", err)
	}
	return resp, cancel
}

// readStreamUntil reads body until want appears, or fails after timeout. It is meant to be
// called once per stream: the reader goroutine may consume bytes past want.
func readStreamUntil(t *testing.T, body io.Reader, want string, timeout time.Duration) string {
	t.Helper()
	type result struct{ text string }
	ch := make(chan result, 1)
	go func() {
		var buf strings.Builder
		tmp := make([]byte, 256)
		for {
			n, err := body.Read(tmp)
			buf.Write(tmp[:n])
			if strings.Contains(buf.String(), want) || err != nil {
				ch <- result{text: buf.String()}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		return r.text
	case <-time.After(timeout):
		t.Fatalf("stream never contained %q within %s", want, timeout)
		return ""
	}
}

// TestTailStreamsThroughTheServer opens a real SSE connection with a viewer token and
// checks the documented headers, the connected comment and an event.
func TestTailStreamsThroughTheServer(t *testing.T) {
	src := &tailSource{lines: make(chan source.LogLine, 4)}
	h := newHarnessWithHandlers(t, nil, tailHandlers(src, time.Hour))
	token := h.token(t, []string{"viewer"}, nil)

	resp, cancel := openTail(t, h, token)
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if xa := resp.Header.Get("X-Accel-Buffering"); xa != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xa)
	}

	text := readStreamUntil(t, resp.Body, ": connected", 3*time.Second)
	if !strings.Contains(text, ": connected") {
		t.Fatalf("stream did not start with a connected comment: %q", text)
	}

	src.lines <- source.LogLine{At: time.Now(), Service: "gateway", Level: "info", Message: "hello"}
	text = readStreamUntil(t, resp.Body, "hello", 3*time.Second)
	if !strings.Contains(text, "data: ") || !strings.Contains(text, `"message":"hello"`) {
		t.Fatalf("event = %q, want a data frame carrying the line", text)
	}
}

// TestTailKeepAlive observes the keep-alive comment with a 50 ms interval.
func TestTailKeepAlive(t *testing.T) {
	src := &tailSource{lines: make(chan source.LogLine)}
	h := newHarnessWithHandlers(t, nil, tailHandlers(src, 50*time.Millisecond))

	resp, cancel := openTail(t, h, h.token(t, []string{"viewer"}, nil))
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	text := readStreamUntil(t, resp.Body, ": keep-alive", 3*time.Second)
	if !strings.Contains(text, ": keep-alive") {
		t.Fatalf("stream did not contain a keep-alive comment: %q", text)
	}
}

// TestTailPerUserCap opens two tails for one subject, asserts a third is 429, and checks a
// slot is freed once a client disconnects.
func TestTailPerUserCap(t *testing.T) {
	src := &tailSource{lines: make(chan source.LogLine)}
	h := newHarnessWithHandlers(t, nil, tailHandlers(src, time.Hour))
	token := h.token(t, []string{"viewer"}, nil)

	first, cancelFirst := openTail(t, h, token)
	defer first.Body.Close()
	second, cancelSecond := openTail(t, h, token)
	defer second.Body.Close()

	third, cancelThird := openTail(t, h, token)
	if third.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third status = %d, want 429", third.StatusCode)
	}
	body, _ := io.ReadAll(third.Body)
	third.Body.Close()
	cancelThird()
	if !strings.Contains(string(body), `"code":"too_many_tails"`) {
		t.Errorf("third body = %s, want too_many_tails", body)
	}

	// Releasing one connection frees its slot; poll until the server has noticed.
	cancelFirst()
	deadline := time.Now().Add(3 * time.Second)
	for {
		again, cancelAgain := openTail(t, h, token)
		if again.StatusCode == http.StatusOK {
			cancelAgain()
			again.Body.Close()
			break
		}
		again.Body.Close()
		cancelAgain()
		if time.Now().After(deadline) {
			t.Fatalf("a freed slot was never reusable")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancelSecond()
}

// TestTailGlobalCap holds ten tails across five subjects and asserts the next subject is
// refused by the global cap.
func TestTailGlobalCap(t *testing.T) {
	src := &tailSource{lines: make(chan source.LogLine)}
	h := newHarnessWithHandlers(t, nil, tailHandlers(src, time.Hour))

	var cancels []context.CancelFunc
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	for i := 0; i < 5; i++ {
		subject := "staff-" + string(rune('a'+i))
		token := h.token(t, []string{"viewer"}, func(c jwt.MapClaims) { c["sub"] = subject })
		for j := 0; j < 2; j++ {
			resp, cancel := openTail(t, h, token)
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				cancel()
				t.Fatalf("tail %d for %s = %d, want 200", j, subject, resp.StatusCode)
			}
			cancels = append(cancels, cancel)
			defer resp.Body.Close()
		}
	}

	other := h.token(t, []string{"viewer"}, func(c jwt.MapClaims) { c["sub"] = "staff-z" })
	resp, cancel := openTail(t, h, other)
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("eleventh status = %d, want 429", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"too_many_tails"`) {
		t.Errorf("eleventh body = %s, want too_many_tails", body)
	}
}
