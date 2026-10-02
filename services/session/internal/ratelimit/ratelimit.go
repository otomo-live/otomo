// Package ratelimit counts actions per player in fixed windows in Valkey, for limits that
// must hold across Session instances (SES-C5: friend requests). It owns the
// ratelimit:* keys.
package ratelimit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"
)

// count adds one to a window's counter, starting the window on the first action, and
// answers how long until the window ends when the limit is passed (0 while under it).
// One script, so the counter and its expiry cannot be split by a crash.
//
// KEYS[1] ratelimit:{action}:{player}
// ARGV[1] limit per window
// ARGV[2] window, milliseconds
const count = `
local n = redis.call('INCR', KEYS[1])
if n == 1 then
	redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
if n > tonumber(ARGV[1]) then
	return math.max(redis.call('PTTL', KEYS[1]), 1)
end
return 0
`

// Limiter is a set of fixed-window counters. Build it with New.
type Limiter struct {
	vk  valkey.Client
	lua *valkey.Lua
}

// New returns a Limiter on vk.
func New(vk valkey.Client) *Limiter {
	return &Limiter{vk: vk, lua: valkey.NewLuaScript(count)}
}

// Allow counts one action by player and returns 0 while they are within limit actions
// per window, or how long until they may act again.
func (l *Limiter) Allow(ctx context.Context, action, player string, limit int, window time.Duration) (time.Duration, error) {
	wait, err := l.lua.Exec(ctx, l.vk, []string{"ratelimit:" + action + ":" + player},
		[]string{strconv.Itoa(limit), strconv.FormatInt(window.Milliseconds(), 10)}).AsInt64()
	if err != nil {
		return 0, fmt.Errorf("ratelimit: count %s for %s: %w", action, player, err)
	}
	return time.Duration(wait) * time.Millisecond, nil
}
