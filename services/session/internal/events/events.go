// Package events is the service's Valkey access layer: the client, the atomic event
// producer of design/04-session-minimal.md §4, and the read half of the long-poll
// protocol.
//
// It owns the keys §3.2 defines — events:{player}, events:seq:{player} and the
// notify:{player} channel — and nothing else. Presence and the online sorted set belong
// to the presence work (SES-B3…B5) and are deliberately not here: this package is what
// the event stream is, and a key it does not name is a key it cannot corrupt.
//
// The durable data is in Postgres. Everything in this package is a hint that expires on
// its own, which is why losing it is survivable: a missed event delays a client's UI by
// one poll, and the client's `resync` response sends it back to the REST reads that own
// the truth.
package events

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go"
)

// The stream's two limits, from §3.2. EventCap is passed to the script as an argument
// rather than written into it, so the number lives in Go where a reader of this package
// will find it, and so a test can exercise trimming with a cap small enough to reach.
const (
	EventCap = 100
	EventTTL = 10 * time.Minute
)

// The closed set of event types in M1 (§4). A type outside this list is refused at the
// call site rather than published: an event nobody handles is invisible to the client
// that receives it, and a typo in a call site is not something a later reader can tell
// from a deliberately new type. Adding a type here is the whole change needed.
const (
	TypeFriendRequest  = "friend.request"
	TypeFriendAccepted = "friend.accepted"
	TypeFriendRemoved  = "friend.removed"
	TypePartyInvite    = "party.invite"
	TypePartyUpdated   = "party.updated"
	TypePartyKicked    = "party.kicked"
	TypePartyDisbanded = "party.disbanded"
	TypePresenceChange = "presence.changed"

	// The launch events (design/14-launch-handoff.md §7).
	TypePartyLaunching    = "party.launching"
	TypePartyLaunchFailed = "party.launch_failed"
	TypePartyReturned     = "party.returned"
)

// KnownType reports whether typ is one of the M1 event types above.
func KnownType(typ string) bool {
	switch typ {
	case TypeFriendRequest, TypeFriendAccepted, TypeFriendRemoved,
		TypePartyInvite, TypePartyUpdated, TypePartyKicked, TypePartyDisbanded,
		TypePresenceChange,
		TypePartyLaunching, TypePartyLaunchFailed, TypePartyReturned:
		return true
	default:
		return false
	}
}

//go:embed publish.lua
var publishScript string

//go:embed read.lua
var readScript string

// Event is one entry in a player's stream.
//
// At is unix milliseconds rather than a time.Time because the producer's script writes
// the document and Lua has no date library: the service supplies the clock, the script
// only formats it. Encoding stays fixed-width and integer for the same reason — a JSON
// number is what Lua can concatenate without knowing anything about time.
type Event struct {
	Seq     int64           `json:"seq"`
	At      int64           `json:"at"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Time returns the event's timestamp as a time.Time in UTC.
func (e Event) Time() time.Time { return time.UnixMilli(e.At).UTC() }

// Client is the Valkey client plus the compiled producer script. Safe for concurrent
// use; build it once per process with New.
type Client struct {
	vk      valkey.Client
	lua     *valkey.Lua
	readLua *valkey.Lua
	now     func() time.Time // overridden in tests; never nil
	ttl     time.Duration
	cap     int
}

// New parses url, builds the client and pings once, so a container with an unreachable or
// malformed Valkey URL fails at start-up rather than on its first event. On failure no
// client is leaked: it is closed before the error is returned.
//
// url carries the ACL username and password (SES-A3 configures Valkey with a password),
// which is why this takes a URL rather than a host and port: the secret travels in the
// one value the deployment already protects, and a test can point at a unix socket
// without a second code path.
func New(ctx context.Context, url string) (*Client, error) {
	opt, err := valkey.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse SESSION_VALKEY_URL: %w", err)
	}
	// This service has no per-reading cache to invalidate: every event read must see the
	// list as it is now, or a long-poll would answer from a stale snapshot. Disabling the
	// client-side cache also means the client never needs the RESP3 invalidation channel,
	// so a Valkey speaking RESP2 works too.
	opt.DisableCache = true

	vk, err := valkey.NewClient(opt)
	if err != nil {
		return nil, fmt.Errorf("create valkey client: %w", err)
	}
	// The dial happens inside NewClient, so this catches what a live connection can still
	// get wrong: a wrong ACL password, or a non-Valkey service on the port. It is the
	// start-up check for SES-A3's credentials, not a second copy of the dial above.
	if err := vk.Do(ctx, vk.B().Ping().Build()).Error(); err != nil {
		vk.Close()
		return nil, fmt.Errorf("ping valkey: %w", err)
	}

	return &Client{
		vk:      vk,
		lua:     valkey.NewLuaScript(publishScript),
		readLua: valkey.NewLuaScript(readScript),
		now:     time.Now,
		ttl:     EventTTL,
		cap:     EventCap,
	}, nil
}

// Close releases the client and its connections. Callers should defer it immediately
// after a successful New, so every later error path unwinds through it.
func (c *Client) Close() {
	c.vk.Close()
}

// Conn returns the underlying Valkey connection, for the presence store (SE-3), which
// shares it and owns its own keys. Close still belongs to this Client.
func (c *Client) Conn() valkey.Client { return c.vk }

// Ping reports whether Valkey is reachable. /readyz calls it once per request, which is
// a single command on a multiplexed connection — cheap enough that caching the result,
// as the Postgres probe does, would only add a way to be wrong.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.vk.Do(ctx, c.vk.B().Ping().Build()).Error(); err != nil {
		return fmt.Errorf("valkey unreachable: %w", err)
	}
	return nil
}

// Publish appends one event to player's stream and returns it, with the sequence number
// the script assigned. It is the `publish_event(player_id, type, payload)` of SES-A4.
//
// The whole of §4's produce step — INCR, RPUSH, LTRIM, EXPIRE, PUBLISH — is one script,
// so two producers racing on the same player cannot take the same sequence number, and
// no caller can be the one that forgets to trim or expire.
//
// Callers must publish *after* the transaction that caused the event has committed
// (SES-D3): an event for a write that rolled back would tell a client to refetch state
// that never changed, which is harmless, and would be indistinguishable from the write
// never having happened, which is not.
func (c *Client) Publish(ctx context.Context, playerID, typ string, payload any) (Event, error) {
	if !KnownType(typ) {
		return Event{}, fmt.Errorf("events: unknown event type %q", typ)
	}
	if playerID == "" {
		return Event{}, errors.New("events: publish without a player id")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("events: encode payload for %s: %w", typ, err)
	}
	if payload == nil {
		body = []byte("{}")
	}

	at := c.now().UnixMilli()

	seq, err := c.lua.Exec(ctx, c.vk,
		[]string{eventsKey(playerID), seqKey(playerID)},
		[]string{
			typ,
			string(body),
			fmt.Sprint(int(c.ttl / time.Second)),
			notifyChannel(playerID),
			fmt.Sprint(at),
			fmt.Sprint(c.cap),
		},
	).ToInt64()
	if err != nil {
		return Event{}, fmt.Errorf("events: publish %s to %s: %w", typ, playerID, err)
	}

	return Event{Seq: seq, At: at, Type: typ, Payload: body}, nil
}

// Read returns the events in player's stream with a sequence number greater than after,
// newest last, and reports whether the client has fallen too far behind to be told the
// truth from this stream alone.
//
// resync is true when `after` is older than the oldest event still retained — the client
// missed events that were trimmed away, so answering with what survives would leave it
// silently wrong. §4's answer is to say so and let it refetch friends and party state
// over REST, which is why this returns a flag rather than an error: nothing failed.
//
// The oldest retained sequence is derived rather than stored. Sequence numbers are
// contiguous and each event is one list element, so the tail of the list is
// seq-LLEN+1 — a second key that had to be kept in step with the first would be one more
// thing the script could get wrong.
//
// The counter and the list are read by one script (read.lua), so a publish cannot land
// between them and make the window look narrower than it is (SE-4).
func (c *Client) Read(ctx context.Context, playerID string, after int64) ([]Event, bool, error) {
	if playerID == "" {
		return nil, false, errors.New("events: read without a player id")
	}

	res, err := c.readLua.Exec(ctx, c.vk,
		[]string{eventsKey(playerID), seqKey(playerID)}, nil).ToArray()
	if err != nil {
		return nil, false, fmt.Errorf("events: read stream of %s: %w", playerID, err)
	}
	if len(res) != 2 {
		return nil, false, fmt.Errorf("events: read stream of %s: script returned %d values, want 2", playerID, len(res))
	}
	seq, err := res[0].AsInt64()
	if err != nil {
		return nil, false, fmt.Errorf("events: read stream counter of %s: %w", playerID, err)
	}
	raw, err := res[1].AsStrSlice()
	if err != nil {
		return nil, false, fmt.Errorf("events: read stream of %s: %w", playerID, err)
	}

	if NeedsResync(after, seq, int64(len(raw))) {
		return nil, true, nil
	}

	out := make([]Event, 0, len(raw))
	for _, item := range raw {
		var ev Event
		if err := json.Unmarshal([]byte(item), &ev); err != nil {
			// A malformed entry is not answered around: it would be skipped silently,
			// and the client's cursor would advance past an event it never saw. Failing
			// the read makes it visible instead, and the stream expires on its own.
			return nil, false, fmt.Errorf("events: decode event %d of %s: %w", len(out), playerID, err)
		}
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out, false, nil
}

// NeedsResync decides whether a client whose cursor is after can be answered from a
// stream whose counter is seq and which still holds length events. It is true when:
//
//   - after is older than the oldest retained event (seq-length+1): events the client
//     never saw were trimmed away;
//   - after is ahead of seq: the stream expired and started again from 1 since the
//     client last read it, so its cursor belongs to a stream that no longer exists and
//     it would otherwise wait for ever for numbers the new stream has already used.
//
// A client at after=0 on an empty stream is simply up to date.
func NeedsResync(after, seq, length int64) bool {
	if after > seq {
		return true
	}
	return after < seq-length
}

// Seq returns the stream's current sequence number, or 0 when it has expired. A client
// reading with `after=Seq()` waits for the next event rather than catching up, which is
// how a fresh connection starts polling without a resync.
func (c *Client) Seq(ctx context.Context, playerID string) (int64, error) {
	seq, err := c.vk.Do(ctx, c.vk.B().Get().Key(seqKey(playerID)).Build()).AsInt64()
	if err != nil && !isNil(err) {
		return 0, fmt.Errorf("events: read stream counter of %s: %w", playerID, err)
	}
	return seq, nil
}

// eventsKey is §3.2's `events:{player_id}`.
func eventsKey(playerID string) string { return "events:" + playerID }

// seqKey is §3.2's `events:seq:{player_id}`.
func seqKey(playerID string) string { return "events:seq:" + playerID }

// notifyChannel is §3.2's `notify:{player_id}` pub/sub channel.
func notifyChannel(playerID string) string { return "notify:" + playerID }

// isNil reports whether err is Valkey's answer for a key that does not exist. The
// distinction matters wherever a missing key is a state — an expired stream — rather
// than a failure.
func isNil(err error) bool {
	return errors.Is(err, valkey.Nil)
}
