// The rules loader (LB-1): session.rules from the live server manifest.

package rules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Namespace is the Config namespace the loader reads.
const Namespace = "session.rules"

// ReleaseHeader carries a release_id: Patch sends it with the server manifest (CF-3), and
// clients send the release they loaded on every Session request (D5, SE-8).
const ReleaseHeader = "X-Otomo-Release"

// RefreshMin is the least time between the end of one poll and a poll Refresh starts,
// and refreshTimeout bounds that poll, since a request is waiting on it.
const (
	RefreshMin     = 5 * time.Second
	refreshTimeout = 2 * time.Second
)

// maxDocumentBytes caps what the loader reads from Patch. A rules document is a few
// hundred bytes; anything near this is not one.
const maxDocumentBytes = 1 << 20

// Poll results, the values of session_rules_polls_total{result}.
const (
	ResultNotModified = "not_modified" // 304: the release did not change
	ResultLoaded      = "loaded"       // new rules are in force
	ResultRejected    = "rejected"     // the document was bad; the last good rules stay
	ResultError       = "error"        // Patch could not be asked; the last good rules stay
)

// Loader is a Source that follows the session.rules document of one channel's current
// release, which Patch serves on its internal listener (CF-3). It starts on the
// compiled-in defaults and only ever replaces them with rules that passed Parse, so
// Current is always a usable set: a bad document, a hash mismatch or Patch being down
// all leave the last good rules in force.
type Loader struct {
	baseURL string
	channel string
	key     string
	client  *http.Client
	every   time.Duration
	log     *slog.Logger

	current atomic.Pointer[Rules]

	mu       sync.Mutex   // guards etag, loaded and lastPoll across polls
	etag     string       // the manifest last dealt with, for If-None-Match
	loaded   string       // sha256 of the document in force, "" for the defaults
	lastPoll time.Time    // when the last poll ended
	head     atomic.Int64 // the channel's live release_id; 0 until Patch has answered
	polls    *prometheus.CounterVec
	release  prometheus.Gauge
}

// LoaderConfig is what NewLoader needs. Key is the text of patch_session.key.
type LoaderConfig struct {
	PatchURL string
	Channel  string
	Key      string
	Interval time.Duration
	Client   *http.Client
	Log      *slog.Logger
}

// NewLoader returns a Loader serving the compiled-in defaults until its first
// successful poll.
func NewLoader(cfg LoaderConfig) *Loader {
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	l := &Loader{
		baseURL: strings.TrimRight(cfg.PatchURL, "/"),
		channel: cfg.Channel,
		key:     cfg.Key,
		client:  client,
		every:   cfg.Interval,
		log:     log,
		polls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "session_rules_polls_total",
			Help: "Polls of the session.rules document, by result (not_modified, loaded, rejected, error).",
		}, []string{"result"}),
		release: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "session_rules_release_id",
			Help: "The release whose session.rules are in force; 0 while on the compiled-in defaults.",
		}),
	}
	for _, r := range []string{ResultNotModified, ResultLoaded, ResultRejected, ResultError} {
		l.polls.WithLabelValues(r)
	}
	l.current.Store(Defaults())
	return l
}

// Current returns the rules in force.
func (l *Loader) Current() *Rules {
	return l.current.Load()
}

// Collectors returns the loader's metrics, for the service's registry.
func (l *Loader) Collectors() []prometheus.Collector {
	return []prometheus.Collector{l.polls, l.release}
}

// Run polls once at once and then every Interval until ctx ends.
func (l *Loader) Run(ctx context.Context) {
	l.Poll(ctx)
	t := time.NewTicker(l.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Poll(ctx)
		}
	}
}

// manifestDoc is the part of the server manifest the loader reads.
type manifestDoc struct {
	ReleaseID int64 `json:"release_id"`
	Config    map[string]struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"config"`
}

// Poll asks Patch for the server manifest once and applies what it finds. It returns
// the result it counted.
func (l *Loader) Poll(ctx context.Context) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pollLocked(ctx)
}

// Head returns the release_id of the channel's live release as Patch last reported it,
// or 0 before Patch has answered once (SE-8).
func (l *Loader) Head() int64 { return l.head.Load() }

// Refresh polls at once and returns the head, unless a poll is running now or one ended
// under RefreshMin ago; then it returns the head as it is. The release check calls it
// when a client's release differs from the head, so a client that patched to a release
// published since the last poll is not refused for up to a poll interval. It never waits
// for another poll, and its own poll gives up after refreshTimeout.
func (l *Loader) Refresh(ctx context.Context) int64 {
	if !l.mu.TryLock() {
		return l.Head()
	}
	defer l.mu.Unlock()
	if time.Since(l.lastPoll) >= RefreshMin {
		ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		defer cancel()
		l.pollLocked(ctx)
	}
	return l.Head()
}

// pollLocked is Poll with l.mu already held.
func (l *Loader) pollLocked(ctx context.Context) string {
	defer func() { l.lastPoll = time.Now() }()

	result, err := l.poll(ctx)
	l.polls.WithLabelValues(result).Inc()
	switch result {
	case ResultRejected:
		l.log.Warn("session.rules rejected; keeping the last good rules", slog.Any("error", err))
	case ResultError:
		if ctx.Err() == nil {
			l.log.Warn("session.rules could not be fetched; keeping the last good rules", slog.Any("error", err))
		}
	}
	return result
}

func (l *Loader) poll(ctx context.Context) (string, error) {
	body, hdr, status, err := l.get(ctx, "/internal/patch/server-manifest/"+l.channel, l.etag)
	if err != nil {
		return ResultError, err
	}
	etag := hdr.Get("ETag")
	if status == http.StatusNotModified {
		return ResultNotModified, nil
	}

	var m manifestDoc
	jsonErr := json.Unmarshal(body, &m)
	// The channel's head (SE-8) is the release Patch serves now, whatever becomes of its
	// rules: X-Otomo-Release (CF-3), or the manifest's own release_id.
	if id, err := strconv.ParseInt(hdr.Get(ReleaseHeader), 10, 64); err == nil && id > 0 {
		l.head.Store(id)
	} else if jsonErr == nil && m.ReleaseID > 0 {
		l.head.Store(m.ReleaseID)
	}
	if err := jsonErr; err != nil {
		// The manifest itself is broken; asking again with the same ETag gets the same
		// bytes, so remember it and wait for the next release.
		l.etag = etag
		return ResultRejected, fmt.Errorf("server manifest: %w", err)
	}

	entry, ok := m.Config[Namespace]
	if !ok {
		// The release carries no session.rules: the compiled-in defaults are the rules.
		l.etag = etag
		if l.loaded != "" {
			l.apply(Defaults(), "", 0)
			return ResultLoaded, nil
		}
		return ResultNotModified, nil
	}
	if entry.SHA256 == l.loaded {
		l.etag = etag
		return ResultNotModified, nil
	}

	doc, _, status, err := l.get(ctx, "/internal/patch/blob/"+entry.SHA256, "")
	if err != nil {
		return ResultError, err
	}
	if status != http.StatusOK {
		return ResultError, fmt.Errorf("blob %s: status %d", entry.SHA256, status)
	}
	sum := sha256.Sum256(doc)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, entry.SHA256) {
		// Not remembered: a torn read could come right on the next poll.
		return ResultRejected, fmt.Errorf("blob %s hashes to %s", entry.SHA256, got)
	}

	r, err := Parse(doc)
	l.etag = etag
	if err != nil {
		return ResultRejected, fmt.Errorf("release %d: %w", m.ReleaseID, err)
	}
	l.apply(r, entry.SHA256, m.ReleaseID)
	return ResultLoaded, nil
}

func (l *Loader) apply(r *Rules, sha string, release int64) {
	l.current.Store(r)
	l.loaded = sha
	l.release.Set(float64(release))
	l.log.Info("session.rules in force",
		slog.Int64("release_id", release),
		slog.Int("party_max_size", r.Party.MaxSize))
}

// get fetches path from Patch's internal listener with the service key. A 304 or 200 is
// returned with its body and headers; any other status is an error.
func (l *Loader) get(ctx context.Context, path, ifNoneMatch string) ([]byte, http.Header, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+path, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+l.key)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, resp.Header, resp.StatusCode, nil
	case http.StatusOK:
	default:
		return nil, nil, resp.StatusCode, fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes+1))
	if err != nil {
		return nil, nil, 0, err
	}
	if len(body) > maxDocumentBytes {
		return nil, nil, 0, errors.New("response larger than " + fmt.Sprint(maxDocumentBytes) + " bytes")
	}
	return body, resp.Header, resp.StatusCode, nil
}
