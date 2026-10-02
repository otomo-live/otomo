package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// fixtureBody reads one testdata file.
func fixtureBody(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

// fixtureServer serves the query_range fixture at every path and counts label-value calls.
func fixtureServer(t *testing.T, rangeFixture string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	rangeBody := fixtureBody(t, rangeFixture)
	labels := fixtureBody(t, "label_values.json")
	var labelCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/loki/api/v1/label/service/values":
			labelCalls.Add(1)
			_, _ = w.Write(labels)
		default:
			_, _ = w.Write(rangeBody)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &labelCalls
}

func TestQueryMergesStreamsNewestFirst(t *testing.T) {
	srv, _ := fixtureServer(t, "query_range.json")
	c := New(srv.URL, srv.Client(), time.Second)

	lines, err := c.Query(context.Background(), source.LogQuery{From: time.Unix(0, 0), To: time.Unix(3, 0), Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %+v", len(lines), lines)
	}
	if !lines[0].At.Equal(time.Unix(0, 1700000002000000000).UTC()) || lines[0].Service != "gateway" {
		t.Errorf("lines[0] = %+v, want the newest gateway line", lines[0])
	}
	if lines[1].Message != "boom" || lines[1].Level != "error" || lines[1].Service != "config" {
		t.Errorf("lines[1] = %+v, want config/error boom", lines[1])
	}
	if lines[2].Message != "hello" || lines[2].Level != "info" {
		t.Errorf("lines[2] = %+v, want gateway/info hello", lines[2])
	}
}

func TestQueryJSONLineFieldsAndMessage(t *testing.T) {
	srv, _ := fixtureServer(t, "query_range.json")
	c := New(srv.URL, srv.Client(), time.Second)

	lines, err := c.Query(context.Background(), source.LogQuery{From: time.Unix(0, 0), To: time.Unix(3, 0), Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	newest := lines[0]
	if newest.Message != "second" {
		t.Errorf("Message = %q, want the JSON line's msg", newest.Message)
	}
	if newest.Fields["request_id"] != "abc" {
		t.Errorf("Fields = %v, want request_id=abc", newest.Fields)
	}
	plain := lines[2]
	if plain.Fields != nil {
		t.Errorf("plain line Fields = %v, want nil", plain.Fields)
	}
	if plain.Message != "hello" {
		t.Errorf("plain line Message = %q, want the raw line", plain.Message)
	}
}

func TestQueryLimitCutsNewest(t *testing.T) {
	srv, _ := fixtureServer(t, "query_range.json")
	c := New(srv.URL, srv.Client(), time.Second)

	lines, err := c.Query(context.Background(), source.LogQuery{From: time.Unix(0, 0), To: time.Unix(3, 0), Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if !lines[0].At.Equal(time.Unix(0, 1700000002000000000).UTC()) {
		t.Errorf("lines[0] = %+v, want the newest line kept", lines[0])
	}
}

func TestQueryErrorBody(t *testing.T) {
	body := fixtureBody(t, "error.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/loki/api/v1/label/service/values" {
			_, _ = w.Write(fixtureBody(t, "label_values.json"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client(), time.Second)
	_, err := c.Query(context.Background(), source.LogQuery{Service: "gateway"})
	if err == nil {
		t.Fatal("Query returned no error for an error envelope")
	}
	if want := "parse error at line 1"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to mention %q", err, want)
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error %q leaks the request URL", err)
	}
}

func TestServicesCache(t *testing.T) {
	srv, labelCalls := fixtureServer(t, "query_range.json")
	c := New(srv.URL, srv.Client(), time.Second)
	c.svcTTL = 50 * time.Millisecond

	set, err := c.Services(context.Background())
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if !set.Contains("gateway") || !set.Contains("config") || set.Contains("nope") {
		t.Errorf("set = %v", set)
	}

	if _, err := c.Services(context.Background()); err != nil {
		t.Fatalf("Services (cached): %v", err)
	}
	if got := labelCalls.Load(); got != 1 {
		t.Fatalf("label values called %d times, want 1 (cache hit)", got)
	}

	time.Sleep(60 * time.Millisecond)
	if _, err := c.Services(context.Background()); err != nil {
		t.Fatalf("Services (expired): %v", err)
	}
	if got := labelCalls.Load(); got != 2 {
		t.Fatalf("label values called %d times after the TTL, want 2", got)
	}
}

// growingServer returns one more line on each query_range call, and re-sends the previous
// lines every time so the client has to skip the boundary.
func growingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var polls atomic.Int64
	base := time.Unix(1700000000, 0).UTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/loki/api/v1/label/service/values" {
			_, _ = w.Write(fixtureBody(t, "label_values.json"))
			return
		}
		p := int(polls.Add(1))
		if p > 5 {
			p = 5
		}
		values := make([][]string, 0, p)
		for i := 0; i < p; i++ {
			at := base.Add(time.Duration(i) * time.Second)
			values = append(values, []string{
				fmt.Sprintf("%d", at.UnixNano()),
				fmt.Sprintf("line-%d", i),
			})
		}
		body, _ := json.Marshal(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "streams",
				"result": []map[string]any{{
					"stream": map[string]string{"service": "gateway", "level": "info"},
					"values": values,
				}},
			},
		})
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

func TestTailDeliversOnceAndStopsOnCancel(t *testing.T) {
	srv, polls := growingServer(t)
	c := New(srv.URL, srv.Client(), time.Second)
	c.pollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan source.LogLine, 32)
	done := make(chan error, 1)
	go func() { done <- c.Tail(ctx, source.LogQuery{From: time.Unix(1700000000, 0), Limit: 10}, out) }()

	seen := map[string]int{}
	deadline := time.After(3 * time.Second)
	for len(seen) < 5 {
		select {
		case line := <-out:
			seen[line.Message]++
		case <-deadline:
			t.Fatalf("only saw %d distinct lines: %v", len(seen), seen)
		}
	}
	for i := 0; i < 5; i++ {
		if seen[fmt.Sprintf("line-%d", i)] != 1 {
			t.Errorf("line-%d delivered %d times, want exactly 1", i, seen[fmt.Sprintf("line-%d", i)])
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Tail returned %v, want nil on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Tail did not return after the context was cancelled")
	}

	// No further polling after cancellation.
	before := polls.Load()
	time.Sleep(30 * time.Millisecond)
	if after := polls.Load(); after != before {
		t.Errorf("polling continued after cancel: %d -> %d", before, after)
	}
}

func TestTailReturnsBuildError(t *testing.T) {
	srv, _ := fixtureServer(t, "query_range.json")
	c := New(srv.URL, srv.Client(), time.Second)

	out := make(chan source.LogLine, 1)
	err := c.Tail(context.Background(), source.LogQuery{Service: "nope"}, out)
	if !errors.Is(err, ErrUnknownService) {
		t.Fatalf("Tail error = %v, want ErrUnknownService", err)
	}
}

// lokiObserver captures the instrumentation seam's callbacks.
type lokiObserver struct {
	mu     sync.Mutex
	events []struct {
		upstream string
		result   string
	}
}

func (o *lokiObserver) UpstreamRequest(upstream, result string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, struct {
		upstream string
		result   string
	}{upstream, result})
}

func (o *lokiObserver) last(t *testing.T) (string, string) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.events) == 0 {
		t.Fatal("no upstream events recorded")
	}
	e := o.events[len(o.events)-1]
	return e.upstream, e.result
}

// TestObserverRecordsUpstreamResult pins loki's side of the seam, including the timeout
// classification the metric relies on.
func TestObserverRecordsUpstreamResult(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv, _ := fixtureServer(t, "query_range.json")
		c := New(srv.URL, srv.Client(), time.Second)
		obs := &lokiObserver{}
		c.Observer = obs

		if _, err := c.queryRange(context.Background(), `{service=~".+"}`, time.Unix(0, 0), time.Unix(3, 0), 10, "backward"); err != nil {
			t.Fatalf("queryRange: %v", err)
		}
		if upstream, result := obs.last(t); upstream != "loki" || result != "ok" {
			t.Fatalf("event = %s/%s, want loki/ok", upstream, result)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(func() {
			close(release)
			srv.Close()
		})

		c := New(srv.URL, srv.Client(), 30*time.Millisecond)
		obs := &lokiObserver{}
		c.Observer = obs

		if _, err := c.queryRange(context.Background(), `{service=~".+"}`, time.Unix(0, 0), time.Unix(3, 0), 10, "backward"); err == nil {
			t.Fatal("expected a timeout error")
		}
		if upstream, result := obs.last(t); upstream != "loki" || result != "timeout" {
			t.Fatalf("event = %s/%s, want loki/timeout", upstream, result)
		}
	})
}
