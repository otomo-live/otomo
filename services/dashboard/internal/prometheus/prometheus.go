// Package prometheus implements source.MetricsSource against Prometheus's HTTP API with
// plain net/http and encoding/json.
//
// It runs queries the Dashboard built from its named template catalogue, and converts the
// responses into the shapes the WebUI consumes: a stable label-string key for instant
// values, and the columnar timestamp/value arrays for range queries
// (design/01-dashboard.md §4 and §4a). It holds no state beyond a cached allow-list.
package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	// UpstreamRequest records one completed request. upstream is the fixed
	// "prometheus"; result is one of ok, error, timeout.
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
	// maxBodyBytes caps how much of a Prometheus response this client will read. A
	// pathological query must not be able to make the Dashboard allocate without bound.
	maxBodyBytes = 16 << 20
	// defaultTimeout keeps a zero-value Client from hanging forever.
	defaultTimeout = 2 * time.Second
	// allowListTTL is how long a fetched allow-list stays fresh. Shorter than this, a
	// call is served from memory; longer, one caller refreshes it for everybody.
	allowListTTL = 30 * time.Second
	// allowListRetry is how soon a failed allow-list refresh is retried. It is much
	// shorter than allowListTTL so a Prometheus blip does not turn every service into
	// an unknown one for half a minute.
	allowListRetry = 2 * time.Second
)

// Client is a Prometheus HTTP API client. It is safe for concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
	timeout time.Duration

	// Observer is optional instrumentation, wired after construction and before the
	// client serves traffic.
	Observer Observer

	allowMu    sync.Mutex
	allowSet   promql.ServiceSet
	allowErr   error
	allowAt    time.Time
	refreshing bool
	refresh    chan struct{}
	// allowTTL is allowListTTL, overridable in tests so cache expiry can be
	// exercised without a 30-second sleep.
	allowTTL   time.Duration
	allowRetry time.Duration
}

// New returns a Client for baseURL. A nil client becomes http.DefaultClient; a
// non-positive timeout becomes defaultTimeout. baseURL is stored with any trailing slash
// removed.
func New(baseURL string, client *http.Client, timeout time.Duration) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		http:       client,
		timeout:    timeout,
		allowTTL:   allowListTTL,
		allowRetry: allowListRetry,
	}
}

// envelope is the common shape of every Prometheus API response. Data is kept raw so each
// query shape can be decoded without an intermediate map[string]any.
type envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
}

// get performs one bounded GET and returns the success envelope's data. Any non-2xx or
// status:"error" becomes an error built from Prometheus's own errorType/error, never from
// the request URL.
func (c *Client) get(ctx context.Context, path string, params url.Values) (data json.RawMessage, err error) {
	start := time.Now()
	defer func() {
		if c.Observer != nil {
			c.Observer.UpstreamRequest("prometheus", upstreamResult(err), time.Since(start))
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
		return nil, fmt.Errorf("prometheus: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: request failed: %w", unwrapURL(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("prometheus: read response: %w", unwrapURL(err))
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("prometheus: response body exceeds %d bytes", maxBodyBytes)
	}

	var env envelope
	decodeErr := json.Unmarshal(body, &env)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if decodeErr == nil && (env.ErrorType != "" || env.Error != "") {
			return nil, apiError(env)
		}
		return nil, fmt.Errorf("prometheus: HTTP %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("prometheus: decode response: %w", decodeErr)
	}
	if env.Status == "error" {
		return nil, apiError(env)
	}
	return env.Data, nil
}

// apiError renders Prometheus's own error fields. It never includes the request URL.
func apiError(env envelope) error {
	kind := env.ErrorType
	if kind == "" {
		kind = "error"
	}
	msg := env.Error
	if msg == "" {
		msg = "upstream returned an error"
	}
	return fmt.Errorf("prometheus: %s: %s", kind, msg)
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

// parseValue decodes one Prometheus sample value. Prometheus encodes it as a JSON string
// so that NaN and +Inf survive; a bare number is accepted too.
func parseValue(raw json.RawMessage) (float64, error) {
	b := strings.TrimSpace(string(raw))
	if b == "" {
		return 0, errors.New("empty value")
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal([]byte(b), &s); err != nil {
			return 0, err
		}
		return strconv.ParseFloat(s, 64)
	}
	return strconv.ParseFloat(b, 64)
}

// seriesKey renders a label set stably: sorted k="v" pairs inside braces, with __name__
// dropped because the metric name is not part of a chart's series identity. An empty set
// renders "{}".
func seriesKey(metric map[string]string) string {
	if len(metric) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(metric))
	for k := range metric {
		if k == "__name__" {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "{}"
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(metric[k])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// LabelValue returns the value of the named label in a series key rendered by seriesKey
// ("{a=\"1\",b=\"2\"}"). It returns "" when the key is not in that shape or does not carry
// the label. The overview batch needs the service and version labels out of the instant
// map's keys, which is cheaper and keeps source.MetricsSource unchanged.
func LabelValue(key, name string) string {
	if len(key) < 2 || key[0] != '{' || key[len(key)-1] != '}' {
		return ""
	}
	inner := key[1 : len(key)-1]
	for inner != "" {
		eq := strings.IndexByte(inner, '=')
		if eq <= 0 {
			return ""
		}
		label := inner[:eq]
		value, rest, ok := quotedValue(inner[eq+1:])
		if !ok {
			return ""
		}
		if label == name {
			return value
		}
		if rest == "" || rest[0] != ',' {
			return ""
		}
		inner = rest[1:]
	}
	return ""
}

// Labels parses a series key rendered by seriesKey ("{a=\"1\",b=\"2\"}") back into its
// label set. A key that is not in that shape yields nil; an unlabelled series ("{}") yields
// an empty, non-nil map. The series handler needs the whole set — not one value — to decide
// whether a series is identified by a single label.
func Labels(key string) map[string]string {
	if len(key) < 2 || key[0] != '{' || key[len(key)-1] != '}' {
		return nil
	}
	inner := key[1 : len(key)-1]
	if inner == "" {
		return map[string]string{}
	}

	out := make(map[string]string)
	for inner != "" {
		eq := strings.IndexByte(inner, '=')
		if eq <= 0 {
			return nil
		}
		label := inner[:eq]
		value, rest, ok := quotedValue(inner[eq+1:])
		if !ok {
			return nil
		}
		out[label] = value
		if rest == "" {
			break
		}
		if rest[0] != ',' {
			return nil
		}
		inner = rest[1:]
	}
	return out
}

// quotedValue reads a leading "..." from s and returns the value and the remainder after
// the closing quote. seriesKey never escapes a value, so this is a plain scan.
func quotedValue(s string) (value, rest string, ok bool) {
	if s == "" || s[0] != '"' {
		return "", "", false
	}
	end := strings.IndexByte(s[1:], '"')
	if end < 0 {
		return "", "", false
	}
	return s[1 : 1+end], s[1+end+1:], true
}

// instantData is the data object of a /api/v1/query response.
type instantData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

// Instant runs one instant query and returns a value per series. A vector yields one entry
// per series; a scalar is keyed "{}". A query matching nothing yields an empty map.
func (c *Client) Instant(ctx context.Context, q string) (map[string]float64, error) {
	raw, err := c.get(ctx, "/api/v1/query", url.Values{"query": {q}})
	if err != nil {
		return nil, err
	}

	var data instantData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("prometheus: decode query data: %w", err)
	}

	out := make(map[string]float64)
	switch data.ResultType {
	case "vector":
		var series []struct {
			Metric map[string]string  `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(data.Result, &series); err != nil {
			return nil, fmt.Errorf("prometheus: decode vector: %w", err)
		}
		for _, s := range series {
			v, err := parseValue(s.Value[1])
			if err != nil {
				return nil, fmt.Errorf("prometheus: decode vector value: %w", err)
			}
			out[seriesKey(s.Metric)] = v
		}
	case "scalar":
		var pair [2]json.RawMessage
		if err := json.Unmarshal(data.Result, &pair); err != nil {
			return nil, fmt.Errorf("prometheus: decode scalar: %w", err)
		}
		v, err := parseValue(pair[1])
		if err != nil {
			return nil, fmt.Errorf("prometheus: decode scalar value: %w", err)
		}
		out["{}"] = v
	}
	return out, nil
}

// rangeData is the data object of a /api/v1/query_range response.
type rangeData struct {
	ResultType string         `json:"resultType"`
	Result     []matrixSeries `json:"result"`
}

// matrixSeries is one series in a matrix result. Values are left as raw JSON pairs so the
// columnar writer can read them directly without an intermediate []map[string]any.
type matrixSeries struct {
	Metric map[string]string    `json:"metric"`
	Values [][2]json.RawMessage `json:"values"`
}

// Range runs one range query and returns the columnar shape the WebUI's uPlot consumes: a
// shared timestamp grid and one value slice per series, every slice the same length as ts.
// A point Prometheus did not return — a gap, or a sample off the requested grid — stays
// NaN so the arrays remain aligned.
func (c *Client) Range(ctx context.Context, q string, from, to time.Time, step time.Duration) ([]int64, map[string][]float64, error) {
	fromSec := from.Unix()
	toSec := to.Unix()
	stepSec := int64(step / time.Second)
	if stepSec < 1 {
		stepSec = 1
	}

	params := url.Values{
		"query": {q},
		"start": {strconv.FormatInt(fromSec, 10)},
		"end":   {strconv.FormatInt(toSec, 10)},
		"step":  {strconv.FormatInt(stepSec, 10)},
	}
	raw, err := c.get(ctx, "/api/v1/query_range", params)
	if err != nil {
		return nil, nil, err
	}

	// The grid is preallocated from the known point count; (to-from)/step+1 points when
	// to is at or after from.
	n := 0
	if toSec >= fromSec {
		n = int((toSec-fromSec)/stepSec) + 1
	}
	ts := make([]int64, n)
	for i := range ts {
		ts[i] = fromSec + int64(i)*stepSec
	}

	var data rangeData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, nil, fmt.Errorf("prometheus: decode range data: %w", err)
	}

	series := make(map[string][]float64, len(data.Result))
	for _, ms := range data.Result {
		vals := make([]float64, n)
		for i := range vals {
			vals[i] = math.NaN()
		}
		for _, pair := range ms.Values {
			t, err := parseValue(pair[0])
			if err != nil {
				continue
			}
			idx := int((t - float64(fromSec)) / float64(stepSec))
			if idx < 0 || idx >= n {
				continue
			}
			// Discard samples that are not exactly on the integer-second grid the
			// caller asked for; writing them at a rounded index would corrupt the
			// timestamps' meaning.
			if fromSec+int64(idx)*stepSec != int64(t) {
				continue
			}
			v, err := parseValue(pair[1])
			if err != nil {
				continue
			}
			vals[idx] = v
		}
		series[seriesKey(ms.Metric)] = vals
	}
	return ts, series, nil
}

// targetsData is the data object of a /api/v1/targets response.
type targetsData struct {
	ActiveTargets []struct {
		Labels     map[string]string `json:"labels"`
		Health     string            `json:"health"`
		LastScrape time.Time         `json:"lastScrape"`
		LastError  string            `json:"lastError"`
	} `json:"activeTargets"`
}

// Targets returns the active scrape targets, one per service label. Targets that do not
// carry a service label are skipped: they are not Dashboard services and have no place on
// /services.
func (c *Client) Targets(ctx context.Context) ([]source.Target, error) {
	raw, err := c.get(ctx, "/api/v1/targets", url.Values{"state": {"active"}})
	if err != nil {
		return nil, err
	}

	var data targetsData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("prometheus: decode targets: %w", err)
	}

	out := make([]source.Target, 0, len(data.ActiveTargets))
	for _, t := range data.ActiveTargets {
		service := t.Labels["service"]
		if service == "" {
			continue
		}
		out = append(out, source.Target{
			Service:     service,
			Up:          t.Health == "up",
			LastScrape:  t.LastScrape,
			ScrapeError: t.LastError,
		})
	}
	return out, nil
}

// alertsData is the data object of a /api/v1/alerts response. value and the rule's own
// extra labels are deliberately not modelled: they are unknown fields and encoding/json
// ignores them.
type alertsData struct {
	Alerts []struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		State       string            `json:"state"`
		ActiveAt    string            `json:"activeAt"`
	} `json:"alerts"`
}

// Alert is one Prometheus alert rule instance. The struct lives in source, beside the
// MetricsSource interface that returns it; this alias keeps the name reachable from the
// package that decodes the API shape.
type Alert = source.Alert

// Alerts returns every alert rule instance Prometheus is tracking, firing and pending. It
// goes through get like the other calls, so the client timeout, error mapping and observer
// all apply unchanged. A query that matches nothing yields an empty, non-nil slice.
func (c *Client) Alerts(ctx context.Context) ([]Alert, error) {
	raw, err := c.get(ctx, "/api/v1/alerts", nil)
	if err != nil {
		return nil, err
	}

	var data alertsData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("prometheus: decode alerts: %w", err)
	}

	out := make([]Alert, 0, len(data.Alerts))
	for _, a := range data.Alerts {
		out = append(out, Alert{
			Name:     a.Labels["alertname"],
			Severity: a.Labels["severity"],
			Service:  a.Labels["service"],
			Summary:  a.Annotations["summary"],
			State:    a.State,
			ActiveAt: a.ActiveAt,
		})
	}
	return out, nil
}

// AllowList returns the set of services currently scraped, cached for allowListTTL. At most
// one refresh is in flight: concurrent callers before the first fetch finishes wait for it
// rather than each issuing a request.
//
// A refresh that fails keeps the last good set and returns it alongside the error; the
// caller decides whether to log it. When there has never been a good set the error stands
// alone, and a caller that ignores it will Build for an empty set, which rejects every
// service.
func (c *Client) AllowList(ctx context.Context) (promql.ServiceSet, error) {
	c.allowMu.Lock()
	// time.Since(zero) is measured in centuries, so an unattempted cache never takes
	// this path; after any attempt — success or failure — the TTL is what suppresses
	// retries.
	if time.Since(c.allowAt) < c.allowTTL {
		set, err := c.allowSet, c.allowErr
		c.allowMu.Unlock()
		return set, err
	}
	if c.refreshing {
		done := c.refresh
		c.allowMu.Unlock()

		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		c.allowMu.Lock()
		set, err := c.allowSet, c.allowErr
		c.allowMu.Unlock()
		return set, err
	}
	c.refreshing = true
	c.refresh = make(chan struct{})
	done := c.refresh
	c.allowMu.Unlock()

	// The refresh is shared by every waiting caller, so it must not die with the
	// context of whichever request happened to start it: detach from cancellation
	// (get still bounds it with the client timeout).
	set, err := c.fetchAllowList(context.WithoutCancel(ctx))

	c.allowMu.Lock()
	if err == nil {
		c.allowSet = set
		c.allowErr = nil
		c.allowAt = time.Now()
	} else {
		// Keep the last good set and retry soon rather than caching the failure for
		// the full TTL.
		c.allowErr = err
		c.allowAt = time.Now().Add(c.allowRetry - c.allowTTL)
	}
	c.refreshing = false
	close(done)
	result, resultErr := c.allowSet, c.allowErr
	c.allowMu.Unlock()

	return result, resultErr
}

// fetchAllowList runs one Targets call and turns it into a ServiceSet. It deliberately runs
// outside allowMu, so the lock only ever guards the cache's pointers and timestamps.
func (c *Client) fetchAllowList(ctx context.Context) (promql.ServiceSet, error) {
	targets, err := c.Targets(ctx)
	if err != nil {
		return nil, err
	}
	set := make(promql.ServiceSet, len(targets))
	for _, t := range targets {
		set[t.Service] = struct{}{}
	}
	return set, nil
}
