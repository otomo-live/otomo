-- One presence heartbeat (SES-B3, SES-B6): refresh the player's lease and their place in
-- the online set, unless their last beat was too recent.
--
-- The rate limit and the write are one script so two beats racing from the same player
-- cannot both pass the check. The last beat's time is the hash's own updated_at, so the
-- limit needs no key of its own and ends when the lease does.
--
-- KEYS[1] presence:{player}   hash: status, updated_at (unix milliseconds)
-- KEYS[2] presence:online     sorted set: player -> last beat (unix milliseconds)
-- ARGV[1] status
-- ARGV[2] now, unix milliseconds
-- ARGV[3] minimum interval between beats, milliseconds
-- ARGV[4] lease TTL, seconds
-- ARGV[5] player id
--
-- Returns {0, wait_ms} when refused, or {1, previous_status} when written; the previous
-- status is "" when the player had no lease, that is, was offline.

local now = tonumber(ARGV[2])
local last = tonumber(redis.call('HGET', KEYS[1], 'updated_at') or '0')
if last > 0 and now - last < tonumber(ARGV[3]) then
	return {0, tonumber(ARGV[3]) - (now - last)}
end

local previous = redis.call('HGET', KEYS[1], 'status') or ''
redis.call('HSET', KEYS[1], 'status', ARGV[1], 'updated_at', ARGV[2])
redis.call('EXPIRE', KEYS[1], ARGV[4])
redis.call('ZADD', KEYS[2], now, ARGV[5])
return {1, previous}
