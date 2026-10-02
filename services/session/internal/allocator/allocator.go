// Package allocator is Session's client for the Allocator's internal API
// (design/14-launch-handoff.md §4.2): reserving a game server for a launching party,
// issuing a fresh join ticket, and reading an allocation back.
//
// Every call presents allocator_session.key (D4). The Allocator has no gateway route;
// Session reaches it on otomo-net only.
package allocator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CallTimeout is doc 14 §6's deadline for one call to the Allocator.
const CallTimeout = 5 * time.Second

// Errors the Allocator answers with, by the codes of doc 14 §4.2.
var (
	ErrNoCapacity       = errors.New("allocator: no free game server (no_capacity)")
	ErrConflict         = errors.New("allocator: the party has a live allocation for other players (allocation_conflict)")
	ErrNotFound         = errors.New("allocator: no such allocation, or the player is not in it (not_found)")
	ErrAllocationEnded  = errors.New("allocator: the allocation is not live (allocation_ended)")
	ErrUnexpectedAnswer = errors.New("allocator: unexpected answer")
	ErrNotConfigured    = errors.New("allocator: no URL or key configured")
)

// Allocation is the answer to POST /internal/allocations: a reserved server behind the
// Gameplay Proxy's public address, and one join ticket per player.
type Allocation struct {
	AllocationID string            `json:"allocation_id"`
	ServerID     string            `json:"server_id"`
	Status       string            `json:"status"`
	Address      string            `json:"address"`
	Port         int               `json:"port"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Tickets      map[string]string `json:"tickets"`
}

// Status is the answer to GET /internal/allocations/{id}.
type Status struct {
	AllocationID string     `json:"allocation_id"`
	PartyID      string     `json:"party_id"`
	ServerID     string     `json:"server_id"`
	Status       string     `json:"status"` // reserved and active are live
	EndReason    string     `json:"end_reason"`
	CreatedAt    time.Time  `json:"created_at"`
	EndedAt      *time.Time `json:"ended_at"`
}

// Live reports whether the allocation still holds a game server.
func (s Status) Live() bool {
	return s.Status == "reserved" || s.Status == "active"
}

// Client calls the Allocator. The zero value is not usable; build it with New.
type Client struct {
	base string
	key  string
	http *http.Client
}

// New returns a client for the Allocator at baseURL, presenting key.
func New(baseURL, key string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), key: key, http: httpClient}
}

// Allocate asks for a game server for partyID and playerIDs. It is idempotent per party
// on the Allocator's side: asking again for the same players returns the same
// allocation with fresh tickets, which is what makes the stuck-launch sweep safe.
func (c *Client) Allocate(ctx context.Context, partyID string, playerIDs []string) (*Allocation, error) {
	var a Allocation
	err := c.do(ctx, http.MethodPost, "/internal/allocations",
		map[string]any{"party_id": partyID, "player_ids": playerIDs}, &a, http.StatusOK, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Ticket issues a fresh join ticket for playerID in allocationID.
func (c *Client) Ticket(ctx context.Context, allocationID, playerID string) (string, time.Time, error) {
	var out struct {
		Ticket    string    `json:"ticket"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	err := c.do(ctx, http.MethodPost, "/internal/allocations/"+url.PathEscape(allocationID)+"/tickets",
		map[string]string{"player_id": playerID}, &out, http.StatusOK)
	return out.Ticket, out.ExpiresAt, err
}

// Get reads an allocation back (the repair poll, LB-4).
func (c *Client) Get(ctx context.Context, allocationID string) (*Status, error) {
	var s Status
	if err := c.do(ctx, http.MethodGet, "/internal/allocations/"+url.PathEscape(allocationID), nil, &s, http.StatusOK); err != nil {
		return nil, err
	}
	return &s, nil
}

// do sends one call with CallTimeout and decodes an accepted answer into out. Known
// error codes become the package's errors; anything else is ErrUnexpectedAnswer.
func (c *Client) do(ctx context.Context, method, path string, body, out any, accept ...int) error {
	if c == nil || c.base == "" || c.key == "" {
		return ErrNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("allocator: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("allocator: read %s %s: %w", method, path, err)
	}

	for _, ok := range accept {
		if resp.StatusCode == ok {
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("%w: %s %s: %v", ErrUnexpectedAnswer, method, path, err)
			}
			return nil
		}
	}

	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	switch e.Error.Code {
	case "no_capacity":
		return ErrNoCapacity
	case "allocation_conflict":
		return ErrConflict
	case "not_found":
		return ErrNotFound
	case "allocation_ended":
		return ErrAllocationEnded
	}
	return fmt.Errorf("%w: %s %s: status %d code %q", ErrUnexpectedAnswer, method, path, resp.StatusCode, e.Error.Code)
}

// TicketExpiry reads a join ticket's exp claim, for the ticket_expires_at clients are
// told (doc 14 §7). The ticket is not verified here: Session only relays it, and the
// proxy and the game server verify it. A ticket whose exp cannot be read gets the zero
// time.
func TicketExpiry(ticket string) time.Time {
	parts := strings.Split(ticket, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0).UTC()
}
