// Package callback delivers end-of-allocation events to Session.
//
// When an allocation ends — the game server reported it, the reaper found the server
// dead, the server re-registered, or a reservation expired — the Allocator flags the
// row callback_pending in the allocation outbox. Session has to be told so it can end
// the party, and this package is the drain: it claims the due rows, POSTs each one to
// Session's ended endpoint, and either clears the flag or schedules a retry.
//
// The outbox, not the HTTP call, is the source of truth. A failed delivery leaves the
// row pending with a backoff, so an Allocator restart or a Session outage does not lose
// the event; after the retries are exhausted the row is left for Session's repair poll
// (GET /internal/allocations?party_id=), which is the recovery path of last resort.
// Claiming uses FOR UPDATE SKIP LOCKED with a lease, so two Allocator instances sharing
// the database never deliver the same row at once.
package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/otomo-live/otomo/services/allocator/internal/metrics"
)

const (
	// claimLimit bounds one Drain so a large backlog does not hold a connection or a
	// request burst for long. The next tick picks up where this one stopped.
	claimLimit = 50

	// requestTimeout bounds one POST to Session. It is the per-request deadline; the
	// client passed to New carries a wider one for the connection as a whole.
	requestTimeout = 5 * time.Second

	// maxAttempts is how many failures an allocation is allowed before the drain gives
	// up and leaves it to Session's repair poll.
	maxAttempts = 5
)

// Notifier is the outbox drain. Build it once with New; it holds no mutable state and
// is safe for concurrent use, though Run drives it from a single goroutine.
type Notifier struct {
	db         *pgxpool.Pool
	sessionURL string
	key        string
	client     *http.Client
	metrics    *metrics.Metrics
	log        *slog.Logger
}

// pendingCallback is one claimed outbox row: the event to deliver and the attempt
// count it had when it was claimed.
type pendingCallback struct {
	allocationID string
	partyID      string
	reason       string
	attempts     int16
}

// endedRequest is the callback body. Session treats the callback as a hint that its
// party should end, so it carries the party and the reason the Allocator ended the
// allocation and nothing else.
type endedRequest struct {
	PartyID string `json:"party_id"`
	Reason  string `json:"reason"`
}

// New returns a drain that talks to Session at sessionURL with key as its bearer token.
// The URL's trailing slash is trimmed so the per-row path is unambiguous. A nil client
// or logger falls back to the process defaults, matching the other packages. m records
// each delivery's delivered/retry/gave-up outcome and may be nil.
func New(db *pgxpool.Pool, sessionURL string, key string, client *http.Client, m *metrics.Metrics, log *slog.Logger) *Notifier {
	if client == nil {
		client = http.DefaultClient
	}
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		db:         db,
		sessionURL: strings.TrimRight(sessionURL, "/"),
		key:        key,
		client:     client,
		metrics:    m,
		log:        log,
	}
}

// Run drains the outbox every interval until ctx is done.
//
// Like the reaper, a drain error does not stop the loop: the likely cause is a brief
// database outage and the next tick is the retry. The final drain's context
// cancellation is the normal shutdown path, not an error to report.
func (n *Notifier) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sent, failed, err := n.Drain(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				n.log.Error("draining the Session callback outbox failed", slog.Any("error", err))
				continue
			}
			if sent > 0 || failed > 0 {
				n.log.Info("drained the Session callback outbox",
					slog.Int("sent", sent),
					slog.Int("failed", failed),
				)
			}
		}
	}
}

// Drain claims up to claimLimit due rows and delivers each one.
//
// The claim is a single statement: it selects the oldest due rows FOR UPDATE SKIP
// LOCKED and immediately pushes their callback_next_at 30 seconds into the future. That
// update is the lease — while it is set, no other drain (in this process or another
// Allocator) will see the row as due — so each claimed row is delivered once. The lease
// is not cleared on failure; the failure path replaces it with the backoff.
//
// sent counts 2xx responses, failed counts everything else (a non-2xx status or a
// network error). A database error while claiming or recording the outcome is returned;
// the per-row delivery failures are not errors, because they are exactly what the
// outbox and the retry schedule exist to absorb.
func (n *Notifier) Drain(ctx context.Context) (sent, failed int, err error) {
	due, err := n.claim(ctx)
	if err != nil {
		return 0, 0, err
	}

	for _, c := range due {
		if err := n.post(ctx, c); err != nil {
			failed++
			attempts := c.attempts + 1
			if uerr := n.recordFailure(ctx, c.allocationID, attempts); uerr != nil {
				return sent, failed, fmt.Errorf("record failed callback for allocation %s: %w", c.allocationID, uerr)
			}
			if attempts >= maxAttempts {
				n.metrics.RecordCallback(metrics.CallbackGaveUp)
				// The row is no longer pending, so this is the last chance to say so
				// in a log. Session's repair poll recovers the event from the
				// allocation row itself.
				n.log.Warn("giving up on a Session allocation callback; Session's repair poll will recover it",
					slog.String("allocation_id", c.allocationID),
					slog.String("party_id", c.partyID),
					slog.Int("attempts", int(attempts)),
					slog.Any("error", err),
				)
			} else {
				n.metrics.RecordCallback(metrics.CallbackRetry)
			}
			continue
		}

		if uerr := n.markDelivered(ctx, c.allocationID); uerr != nil {
			return sent, failed, fmt.Errorf("clear callback for allocation %s: %w", c.allocationID, uerr)
		}
		n.metrics.RecordCallback(metrics.CallbackDelivered)
		sent++
	}
	return sent, failed, nil
}

// claim takes the lease on the due rows and returns them. The subquery's ORDER BY
// ended_at keeps the oldest ended allocation first, and SKIP LOCKED is what makes two
// concurrent drains partition the backlog instead of both claiming it.
func (n *Notifier) claim(ctx context.Context) ([]pendingCallback, error) {
	rows, err := n.db.Query(ctx, `
		UPDATE allocation
		SET callback_next_at = now() + interval '30 seconds'
		WHERE allocation_id IN (
			SELECT allocation_id
			FROM allocation
			WHERE callback_pending
			  AND (callback_next_at IS NULL OR callback_next_at <= now())
			ORDER BY ended_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING allocation_id::text, party_id::text, end_reason, callback_attempts`, claimLimit)
	if err != nil {
		return nil, fmt.Errorf("claim pending Session callbacks: %w", err)
	}
	defer rows.Close()

	var due []pendingCallback
	for rows.Next() {
		var c pendingCallback
		if err := rows.Scan(&c.allocationID, &c.partyID, &c.reason, &c.attempts); err != nil {
			return nil, fmt.Errorf("read a claimed Session callback: %w", err)
		}
		due = append(due, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim pending Session callbacks: %w", err)
	}
	return due, nil
}

// post delivers one callback. A nil error means Session answered 2xx; every other
// outcome is an error the caller records and schedules a retry for. The key is set as
// a header and never appears in a log or an error.
func (n *Notifier) post(ctx context.Context, c pendingCallback) error {
	body, err := json.Marshal(endedRequest{PartyID: c.partyID, Reason: c.reason})
	if err != nil {
		return fmt.Errorf("encode Session callback for allocation %s: %w", c.allocationID, err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/internal/session/allocations/%s/ended", n.sessionURL, c.allocationID)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build Session callback for allocation %s: %w", c.allocationID, err)
	}
	req.Header.Set("Authorization", "Bearer "+n.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("post Session callback for allocation %s: %w", c.allocationID, err)
	}
	defer resp.Body.Close()
	// Drain and discard the body so the connection can be reused; the payload is a
	// bare status and is not read.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("Session answered %s for allocation %s", resp.Status, c.allocationID)
	}
	return nil
}

// markDelivered clears the outbox flag after Session acknowledged the callback. The
// attempt count is left as it was: the row is done, and the number only mattered while
// it was still being retried.
func (n *Notifier) markDelivered(ctx context.Context, allocationID string) error {
	_, err := n.db.Exec(ctx, `
		UPDATE allocation
		SET callback_pending = false, callback_next_at = NULL
		WHERE allocation_id = $1::text::uuid`, allocationID)
	return err
}

// recordFailure advances the attempt count and either schedules the next try or, once
// the attempts are exhausted, clears the flag so the row leaves the outbox.
//
// Attempts 1..4 back off by 1, 2, 4 and 8 seconds — doubling, small enough to recover
// quickly from a blip and large enough not to hammer a Session that is restarting. At
// attempt 5 the drain gives up; Session's repair poll is the recovery path.
func (n *Notifier) recordFailure(ctx context.Context, allocationID string, attempts int16) error {
	if attempts >= maxAttempts {
		_, err := n.db.Exec(ctx, `
			UPDATE allocation
			SET callback_attempts = $2, callback_pending = false, callback_next_at = NULL
			WHERE allocation_id = $1::text::uuid`, allocationID, attempts)
		return err
	}

	_, err := n.db.Exec(ctx, `
		UPDATE allocation
		SET callback_attempts = $2,
		    callback_next_at = now() + make_interval(secs => $3)
		WHERE allocation_id = $1::text::uuid`,
		allocationID, attempts, retryBackoff(attempts).Seconds())
	return err
}

// retryBackoff is the delay before the next attempt for a failure that leaves the
// attempt count at attempts (1..4): 1, 2, 4, 8 seconds.
func retryBackoff(attempts int16) time.Duration {
	return time.Duration(1<<(attempts-1)) * time.Second
}
