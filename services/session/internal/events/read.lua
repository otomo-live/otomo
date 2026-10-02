-- Read a player's stream in one atomic step (SE-4).
--
-- The counter and the list have to be read together. Read separately, a publish landing
-- between the two makes the retained window look one event narrower than it is, and a
-- client at the edge of it is told to resync for nothing.
--
-- KEYS[1] events:{player}      the list of JSON events, oldest first
-- KEYS[2] events:seq:{player}  the counter
--
-- Returns {seq, events}: seq is 0 when the counter does not exist.

local seq = tonumber(redis.call('GET', KEYS[2])) or 0
local events = redis.call('LRANGE', KEYS[1], 0, -1)
return {seq, events}
