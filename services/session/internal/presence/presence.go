// Package presence is who is online (SE-3; design/04-session-minimal.md §3.2,
// SES-B3 to SES-B6).
//
// A player is online while they hold a lease: the hash presence:{player}, refreshed by
// each heartbeat and expiring TTL after the last one. presence:online is a sorted set of
// every player by the time of their last beat, so the online count is one ZCOUNT rather
// than a scan, and the trim loop can find who went offline. Both are hints in Valkey:
// losing them shows players offline until their next beat.
//
// It shares the Valkey connection with the event stream (internal/events) and owns only
// the presence:* keys.
package presence

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"
)

// The timings of doc 04 §3.2 and SES-B4 to SES-B6.
const (
	TTL          = 60 * time.Second // a lease lasts this long after the last beat
	MinInterval  = 10 * time.Second // at most one beat per player per interval
	TrimInterval = 15 * time.Second // how often the online set is trimmed and counted
)

// The statuses a client may send, and offline, which only Session reports: a player
// without a lease.
const (
	StatusOnline  = "online"
	StatusInMenus = "in_menus"
	StatusAway    = "away"
	StatusOffline = "offline"
)

// ValidStatus reports whether a client may send status in a heartbeat.
func ValidStatus(status string) bool {
	switch status {
	case StatusOnline, StatusInMenus, StatusAway:
		return true
	}
	return false
}

// onlineKey is §3.2's presence:online.
const onlineKey = "presence:online"

// leaseKey is §3.2's presence:{player_id}.
func leaseKey(player string) string { return "presence:" + player }

//go:embed heartbeat.lua
var heartbeatScript string

//go:embed trim.lua
var trimScript string

// Beat is the outcome of one heartbeat.
type Beat struct {
	// RetryAfter is how long until the player may beat again. When it is above zero,
	// the beat was refused and nothing changed.
	RetryAfter time.Duration
	// Previous is the status before this beat: StatusOffline when the player held no
	// lease.
	Previous string
}

// Changed reports whether a written beat changed the player's status.
func (b Beat) Changed(status string) bool { return b.RetryAfter == 0 && b.Previous != status }

// Store reads and writes presence in Valkey. Build it with New.
type Store struct {
	vk   valkey.Client
	beat *valkey.Lua
	trim *valkey.Lua
	now  func() time.Time
}

// New returns a Store on vk, the connection the event stream uses.
func New(vk valkey.Client) *Store {
	return &Store{
		vk:   vk,
		beat: valkey.NewLuaScript(heartbeatScript),
		trim: valkey.NewLuaScript(trimScript),
		now:  time.Now,
	}
}

// Heartbeat refreshes player's lease with status, unless their last beat was less than
// MinInterval ago (then Beat.RetryAfter says how long to wait).
func (s *Store) Heartbeat(ctx context.Context, player, status string) (Beat, error) {
	res, err := s.beat.Exec(ctx, s.vk,
		[]string{leaseKey(player), onlineKey},
		[]string{
			status,
			strconv.FormatInt(s.now().UnixMilli(), 10),
			strconv.FormatInt(MinInterval.Milliseconds(), 10),
			strconv.FormatInt(int64(TTL/time.Second), 10),
			player,
		}).ToArray()
	if err != nil {
		return Beat{}, fmt.Errorf("presence: heartbeat of %s: %w", player, err)
	}
	if len(res) != 2 {
		return Beat{}, fmt.Errorf("presence: heartbeat of %s: script returned %d values, want 2", player, len(res))
	}
	written, err := res[0].AsInt64()
	if err != nil {
		return Beat{}, fmt.Errorf("presence: heartbeat of %s: %w", player, err)
	}
	if written == 0 {
		wait, err := res[1].AsInt64()
		if err != nil {
			return Beat{}, fmt.Errorf("presence: heartbeat of %s: %w", player, err)
		}
		return Beat{RetryAfter: max(time.Duration(wait)*time.Millisecond, time.Millisecond)}, nil
	}
	previous, err := res[1].ToString()
	if err != nil {
		return Beat{}, fmt.Errorf("presence: heartbeat of %s: %w", player, err)
	}
	if previous == "" {
		previous = StatusOffline
	}
	return Beat{Previous: previous}, nil
}

// Statuses returns the status of each of players, StatusOffline for a player with no
// lease, in one pipeline of HGETALLs (one round trip however many players).
func (s *Store) Statuses(ctx context.Context, players []string) (map[string]string, error) {
	out := make(map[string]string, len(players))
	if len(players) == 0 {
		return out, nil
	}
	cmds := make(valkey.Commands, len(players))
	for i, p := range players {
		cmds[i] = s.vk.B().Hgetall().Key(leaseKey(p)).Build()
	}
	for i, res := range s.vk.DoMulti(ctx, cmds...) {
		lease, err := res.AsStrMap()
		if err != nil && !errors.Is(err, valkey.Nil) {
			return nil, fmt.Errorf("presence: read statuses: %w", err)
		}
		status := lease["status"]
		if status == "" {
			status = StatusOffline
		}
		out[players[i]] = status
	}
	return out, nil
}

// Trim removes the players whose last beat is older than TTL from the online set and
// returns them: they have just gone offline.
func (s *Store) Trim(ctx context.Context) ([]string, error) {
	cutoff := s.now().Add(-TTL).UnixMilli()
	gone, err := s.trim.Exec(ctx, s.vk, []string{onlineKey},
		[]string{strconv.FormatInt(cutoff, 10)}).AsStrSlice()
	if err != nil {
		return nil, fmt.Errorf("presence: trim the online set: %w", err)
	}
	return gone, nil
}

// Count returns how many players beat within the last TTL: ZCOUNT presence:online
// (now-TTL +inf. It does not depend on the trim having run.
func (s *Store) Count(ctx context.Context) (int64, error) {
	cutoff := s.now().Add(-TTL).UnixMilli()
	n, err := s.vk.Do(ctx, s.vk.B().Zcount().Key(onlineKey).
		Min("("+strconv.FormatInt(cutoff, 10)).Max("+inf").Build()).AsInt64()
	if err != nil {
		return 0, fmt.Errorf("presence: count online players: %w", err)
	}
	return n, nil
}
