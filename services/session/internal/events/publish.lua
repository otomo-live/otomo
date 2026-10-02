-- Append one event to a player's stream and wake anyone waiting on it.
--
-- SES-A4: sequence assignment and the append have to be one atomic step, or two
-- producers racing on the same player could take the same sequence number and the
-- client's `after` cursor would skip or repeat an event. A script is the shape that
-- gives that, in one round trip, without a WATCH/EXEC retry loop.
--
-- KEYS[1] events:{player}   the list of JSON events, newest at the tail
-- KEYS[2] events:seq:{player}  monotonic counter
-- ARGV[1] event type, e.g. "party.invite"
-- ARGV[2] payload, already-encoded JSON ({} when there is none)
-- ARGV[3] TTL in seconds for both keys
-- ARGV[4] pub/sub channel to signal, notify:{player}
-- ARGV[5] wall-clock time in unix milliseconds
-- ARGV[6] maximum events to retain
--
-- The body is assembled here rather than passed in, because the sequence number does
-- not exist until the INCR below: a caller cannot know it. cjson.encode escapes the
-- type string, and the payload arrives as JSON from json.Marshal, so neither is text
-- interpolated into a document it could break out of.

local seq = redis.call('INCR', KEYS[2])

local body = '{"seq":' .. seq
	.. ',"at":' .. ARGV[5]
	.. ',"type":' .. cjson.encode(ARGV[1])
	.. ',"payload":' .. ARGV[2]
	.. '}'

redis.call('RPUSH', KEYS[1], body)
redis.call('LTRIM', KEYS[1], -tonumber(ARGV[6]), -1)

-- Both keys expire together, ten minutes after the last event (design/04-session-minimal.md
-- §3.2). Refreshing on every publish is what makes that "since the last event" rather
-- than "since the first" — a player still receiving events keeps their stream alive, and
-- one who has gone quiet stops costing memory.
redis.call('EXPIRE', KEYS[1], ARGV[3])
redis.call('EXPIRE', KEYS[2], ARGV[3])

-- The wake signal carries the sequence number rather than the event: a waiter re-reads
-- the list, so the message only has to exist. It is still sent with a payload because
-- PUBLISH costs the same either way and the number is useful in a monitor.
redis.call('PUBLISH', ARGV[4], seq)

return seq
