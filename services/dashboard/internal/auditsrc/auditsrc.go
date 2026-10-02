// Package auditsrc is the Dashboard's client for one upstream audit feed.
//
// Config and PHP Admin Auth each expose a page of audit entries at their own path, in the
// same wire shape; the Dashboard merges the two into GET /audit. This package speaks that
// shape and nothing else. It forwards the caller's own bearer token, because the two feeds
// enforce their own access rules — the Dashboard must not widen or reinterpret them.
package auditsrc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Observer receives one instrumentation event per upstream HTTP request. It is the seam
// that keeps this package from importing a metrics library; the server layer implements it.
// A nil Observer is valid and means no instrumentation.
type Observer interface {
	// UpstreamRequest records one completed request. upstream is "config_audit",
	// "admin_auth_audit" or "session_audit"; result is one of ok, error, timeout.
	UpstreamRequest(upstream, result string, d time.Duration)
}

// upstreamName maps a feed's configured name onto the closed metric label set.
func upstreamName(name string) string {
	switch name {
	case "admin-auth":
		return "admin_auth_audit"
	case "session":
		return "session_audit"
	default:
		return "config_audit"
	}
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
	// maxBodyBytes caps how much of an upstream page this client will read. The page is
	// bounded by the upstream's own limit (<=200), so 4 MiB is generous; it exists so a
	// misbehaving upstream cannot make the Dashboard allocate without bound.
	maxBodyBytes = 4 << 20
	// defaultTimeout keeps a zero-value Client from hanging forever.
	defaultTimeout = 2 * time.Second
)

// Entry is one audit row exactly as an upstream sent it. Source is carried through
// verbatim: the merge needs to attribute the row to its feed, but the field is the
// upstream's, not the Dashboard's.
type Entry struct {
	ID        int64           `json:"id"`
	At        time.Time       `json:"at"`
	ActorID   string          `json:"actor_id"`
	ActorName string          `json:"actor_name"`
	Source    string          `json:"source"`
	Action    string          `json:"action"`
	Target    string          `json:"target"`
	Details   json.RawMessage `json:"details"`
}

// Page is one upstream page: at most the requested number of entries, newest first, plus
// the opaque cursor for the next page. NextCursor is "" when the upstream reported null,
// which means the feed is exhausted.
type Page struct {
	Entries    []Entry
	NextCursor string
}

// Query is the validated, parameterised form of one upstream page request. From and To
// are RFC3339 strings already checked by the handler; they are passed through, never
// parsed here.
type Query struct {
	Limit  int
	Cursor string
	Actor  string
	From   string
	To     string
}

// StatusError is an upstream HTTP status the caller should see as its own: 401 or 403 from
// a feed means this caller's token does not grant access to that feed.
type StatusError struct {
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("audit upstream returned HTTP %d", e.Status)
}

// Client is one upstream audit feed: a name used in degraded lists and merge tie-breaks, a
// base URL, and the path of the feed on it. It is safe for concurrent use.
type Client struct {
	name    string
	baseURL string
	path    string
	http    *http.Client
	timeout time.Duration

	// Observer is optional instrumentation, wired after construction and before the
	// client serves traffic.
	Observer Observer
}

// New returns a Client for one feed. A nil client becomes http.DefaultClient; a
// non-positive timeout becomes defaultTimeout. baseURL is stored without a trailing slash
// and path is forced to have a leading one, so the two cannot produce a doubled or missing
// separator.
func New(name, baseURL, path string, client *http.Client, timeout time.Duration) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return &Client{
		name:    name,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		path:    path,
		http:    client,
		timeout: timeout,
	}
}

// Name is the feed's configured name ("config", "admin-auth").
func (c *Client) Name() string { return c.name }

// page is the upstream wire shape. NextCursor is a pointer so an explicit null is
// distinguishable from the empty string.
type page struct {
	Entries    []Entry `json:"entries"`
	NextCursor *string `json:"next_cursor"`
}

// Fetch performs one bounded page request with the caller's token. Only the token is
// copied from the caller's request; no other header travels with it. A 401 or 403 becomes
// a *StatusError carrying that status, so the handler can hand the caller the same answer;
// every other failure is a plain per-source error.
func (c *Client) Fetch(ctx context.Context, token string, q Query) (result Page, err error) {
	start := time.Now()
	defer func() {
		if c.Observer != nil {
			c.Observer.UpstreamRequest(upstreamName(c.name), upstreamResult(err), time.Since(start))
		}
	}()

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	params := url.Values{}
	params.Set("limit", strconv.Itoa(q.Limit))
	if q.Cursor != "" {
		params.Set("cursor", q.Cursor)
	}
	if q.Actor != "" {
		params.Set("actor", q.Actor)
	}
	if q.From != "" {
		params.Set("from", q.From)
	}
	if q.To != "" {
		params.Set("to", q.To)
	}

	endpoint := c.baseURL + c.path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Page{}, fmt.Errorf("auditsrc %s: build request: %w", c.name, err)
	}
	// The caller's credential, verbatim. Deliberately not `req.Header = r.Header.Clone()`:
	// nothing else the browser sent (cookies, trace headers, a spoofed X-Forwarded-*) has
	// any business reaching the upstream.
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("auditsrc %s: request failed: %w", c.name, unwrapURL(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return Page{}, fmt.Errorf("auditsrc %s: read response: %w", c.name, unwrapURL(err))
	}
	if len(body) > maxBodyBytes {
		return Page{}, fmt.Errorf("auditsrc %s: response body exceeds %d bytes", c.name, maxBodyBytes)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Page{}, &StatusError{Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Page{}, fmt.Errorf("auditsrc %s: HTTP %d", c.name, resp.StatusCode)
	}

	var p page
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Page{}, fmt.Errorf("auditsrc %s: decode response: %w", c.name, err)
	}
	// A second value after the first is not a page this client wrote a decoder for.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Page{}, fmt.Errorf("auditsrc %s: trailing data after page", c.name)
	}

	out := Page{Entries: p.Entries}
	if out.Entries == nil {
		out.Entries = []Entry{}
	}
	if p.NextCursor != nil {
		out.NextCursor = *p.NextCursor
	}
	return out, nil
}

// unwrapURL strips the *url.Error wrapper, whose Error() embeds the request URL; the inner
// error still satisfies errors.Is for context.DeadlineExceeded.
func unwrapURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}
