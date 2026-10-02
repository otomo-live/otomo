package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

// Observer receives one instrumentation event per upstream HTTP request. It is the seam
// that keeps this package from importing a metrics library; the server layer implements it.
// A nil Observer is valid and means no instrumentation.
type Observer interface {
	// UpstreamRequest records one completed request. upstream is the fixed "loki";
	// result is one of ok, error, timeout.
	UpstreamRequest(upstream, result string, d time.Duration)
}

// upstreamResult classifies a request error for the upstream metrics: a nil error is ok, a
// deadline (context or transport) is timeout, anything else is error.
func upstreamResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return "timeout"
		}
		return "error"
	}
}

const (
	// maxBodyBytes caps how much of a Loki response this client will read. A query with a
	// huge limit must not be able to make the Dashboard allocate without bound.
	maxBodyBytes = 16 << 20
	// defaultTimeout keeps a zero-value Client from hanging forever.
	defaultTimeout = 2 * time.Second
	// servicesTTL is how long the service label-value list stays fresh.
	servicesTTL = 30 * time.Second
	// servicesRetry is how soon a failed refresh is retried, much shorter than the TTL so
	// a Loki blip does not make every service look unknown for half a minute.
	servicesRetry = 2 * time.Second
	// defaultPollInterval is how often Tail re-queries query_range.
	defaultPollInterval = 1500 * time.Millisecond
	// defaultLimit bounds a query the caller did not bound itself.
	defaultLimit = 200
)

// Client is a Loki HTTP API client. It is safe for concurrent use.
type Client struct {
	baseURL      string
	http         *http.Client
	timeout      time.Duration
	pollInterval time.Duration

	// Observer is optional instrumentation, wired after construction and before the
	// client serves traffic.
	Observer Observer

	svcMu         sync.Mutex
	svcSet        promql.ServiceSet
	svcErr        error
	svcAt         time.Time
	svcRefreshing bool
	svcRefresh    chan struct{}
	// svcTTL and svcRetry mirror the constants and are overridable in tests so cache
	// expiry can be exercised without a sleep.
	svcTTL   time.Duration
	svcRetry time.Duration
}

// New returns a Client for baseURL. A nil client becomes http.DefaultClient; a non-positive
// timeout becomes defaultTimeout. baseURL is stored with any trailing slash removed.
func New(baseURL string, client *http.Client, timeout time.Duration) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		http:         client,
		timeout:      timeout,
		pollInterval: defaultPollInterval,
		svcTTL:       servicesTTL,
		svcRetry:     servicesRetry,
	}
}

// envelope is the common shape of every Loki API response. Data is kept raw so each query
// shape can be decoded without an intermediate map[string]any.
type envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
}

// get performs one bounded GET and returns the success envelope's data. Any non-2xx or
// status:"error" becomes an error built from Loki's own errorType/error, never from the
// request URL.
func (c *Client) get(ctx context.Context, path string, params url.Values) (data json.RawMessage, err error) {
	start := time.Now()
	defer func() {
		if c.Observer != nil {
			c.Observer.UpstreamRequest("loki", upstreamResult(err), time.Since(start))
		}
	}()

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	endpoint := c.baseURL + path
	if q := params.Encode(); q != "" {
		endpoint += "?" + q
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("loki: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki: request failed: %w", unwrapURL(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("loki: read response: %w", unwrapURL(err))
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("loki: response body exceeds %d bytes", maxBodyBytes)
	}

	var env envelope
	decodeErr := json.Unmarshal(body, &env)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if decodeErr == nil && (env.ErrorType != "" || env.Error != "") {
			return nil, apiError(env)
		}
		return nil, fmt.Errorf("loki: HTTP %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("loki: decode response: %w", decodeErr)
	}
	if env.Status == "error" {
		return nil, apiError(env)
	}
	return env.Data, nil
}

// apiError renders Loki's own error fields. It never includes the request URL.
func apiError(env envelope) error {
	kind := env.ErrorType
	if kind == "" {
		kind = "error"
	}
	msg := env.Error
	if msg == "" {
		msg = "upstream returned an error"
	}
	return fmt.Errorf("loki: %s: %s", kind, msg)
}

// unwrapURL strips the *url.Error wrapper, whose Error() embeds the request URL and whose
// message would otherwise end up in a user-visible panel. The inner error still satisfies
// errors.Is for context.DeadlineExceeded.
func unwrapURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// streamResult is one stream in a query_range result.
type streamResult struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}

// queryRangeData is the data object of a /loki/api/v1/query_range response.
type queryRangeData struct {
	ResultType string         `json:"resultType"`
	Result     []streamResult `json:"result"`
}

// queryRange runs one query_range request and returns lines ordered by direction: newest
// first for "backward", oldest first for "forward". limit is applied after the merge so a
// server that ignores it cannot make the response unbounded.
func (c *Client) queryRange(ctx context.Context, logql string, from, to time.Time, limit int, direction string) ([]source.LogLine, error) {
	params := url.Values{
		"query":     {logql},
		"start":     {strconv.FormatInt(from.UnixNano(), 10)},
		"end":       {strconv.FormatInt(to.UnixNano(), 10)},
		"limit":     {strconv.Itoa(limit)},
		"direction": {direction},
	}
	raw, err := c.get(ctx, "/loki/api/v1/query_range", params)
	if err != nil {
		return nil, err
	}

	var data queryRangeData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("loki: decode query_range data: %w", err)
	}

	var out []source.LogLine
	for _, stream := range data.Result {
		service := stream.Stream["service"]
		level := stream.Stream["level"]
		for _, v := range stream.Values {
			if len(v) < 2 {
				continue
			}
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				continue
			}
			line := source.LogLine{
				At:      time.Unix(0, ns).UTC(),
				Service: service,
				Level:   level,
				Message: v[1],
			}
			if obj := parseJSONObject(v[1]); obj != nil {
				line.Fields = obj
				if msg, ok := obj["msg"].(string); ok {
					line.Message = msg
				}
			}
			out = append(out, line)
		}
	}

	descending := direction == "backward"
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			if descending {
				return out[i].At.After(out[j].At)
			}
			return out[i].At.Before(out[j].At)
		}
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Message < out[j].Message
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// parseJSONObject parses line as a JSON object. A line that is not an object — plain text,
// an array, a bare scalar — yields nil, which is what tells the caller to keep the raw line
// as the message.
func parseJSONObject(line string) map[string]any {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return nil
	}
	return obj
}

// Query returns matching lines newest first, at most q.Limit of them.
func (c *Client) Query(ctx context.Context, q source.LogQuery) ([]source.LogLine, error) {
	services, err := c.Services(ctx)
	if err != nil && len(services) == 0 && q.Service != "" {
		return nil, err
	}
	logql, err := Build(q, services)
	if err != nil {
		return nil, err
	}

	from, to := q.From, q.To
	if to.IsZero() {
		to = time.Now()
	}
	if from.IsZero() {
		from = to.Add(-time.Hour)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	return c.queryRange(ctx, logql, from, to, limit, "backward")
}

// Tail polls query_range until ctx is cancelled and sends each new line on out. It never
// closes out; the caller owns the channel.
func (c *Client) Tail(ctx context.Context, q source.LogQuery, out chan<- source.LogLine) error {
	services, err := c.Services(ctx)
	if err != nil && len(services) == 0 && q.Service != "" {
		return err
	}
	logql, err := Build(q, services)
	if err != nil {
		return err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	since := q.From
	if since.IsZero() {
		since = time.Now()
	}

	var lastTS time.Time
	lastLines := map[string]struct{}{}

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		lines, err := c.queryRange(ctx, logql, since, time.Now(), limit, "forward")
		if err == nil {
			for _, line := range lines {
				// The server may return lines from before the cursor (a fixture that
				// ignores start); they were already delivered.
				if line.At.Before(since) {
					continue
				}
				if line.At.After(lastTS) {
					lastTS = line.At
					lastLines = map[string]struct{}{}
				}
				// At the boundary Loki may return the last timestamp again; the same
				// (ts, line) pair must not be delivered twice.
				if _, dup := lastLines[line.Message]; dup {
					continue
				}
				lastLines[line.Message] = struct{}{}

				select {
				case out <- line:
				case <-ctx.Done():
					return nil
				}
			}
			if lastTS.After(since) {
				since = lastTS.Add(time.Nanosecond)
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Services returns the set of service label values Loki currently exposes, cached for
// servicesTTL. At most one refresh is in flight: concurrent callers before the first fetch
// finishes wait for it rather than each issuing a request.
//
// A refresh that fails keeps the last good set and returns it alongside the error; the
// caller decides whether to log it. When there has never been a good set the error stands
// alone.
func (c *Client) Services(ctx context.Context) (promql.ServiceSet, error) {
	c.svcMu.Lock()
	if time.Since(c.svcAt) < c.svcTTL {
		set, err := c.svcSet, c.svcErr
		c.svcMu.Unlock()
		return set, err
	}
	if c.svcRefreshing {
		done := c.svcRefresh
		c.svcMu.Unlock()

		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		c.svcMu.Lock()
		set, err := c.svcSet, c.svcErr
		c.svcMu.Unlock()
		return set, err
	}
	c.svcRefreshing = true
	c.svcRefresh = make(chan struct{})
	done := c.svcRefresh
	c.svcMu.Unlock()

	// The refresh is shared by every waiting caller, so it must not die with the context
	// of whichever request happened to start it: detach from cancellation (get still
	// bounds it with the client timeout).
	set, err := c.fetchServices(context.WithoutCancel(ctx))

	c.svcMu.Lock()
	if err == nil {
		c.svcSet = set
		c.svcErr = nil
		c.svcAt = time.Now()
	} else {
		c.svcErr = err
		c.svcAt = time.Now().Add(c.svcRetry - c.svcTTL)
	}
	c.svcRefreshing = false
	close(done)
	result, resultErr := c.svcSet, c.svcErr
	c.svcMu.Unlock()

	return result, resultErr
}

// fetchServices runs one label-values call and turns it into a ServiceSet. It deliberately
// runs outside svcMu, so the lock only ever guards the cache's pointers and timestamps.
func (c *Client) fetchServices(ctx context.Context) (promql.ServiceSet, error) {
	raw, err := c.get(ctx, "/loki/api/v1/label/service/values", nil)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("loki: decode service values: %w", err)
	}
	set := make(promql.ServiceSet, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set, nil
}
