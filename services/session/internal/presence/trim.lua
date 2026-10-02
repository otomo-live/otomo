-- Remove the players whose last beat is older than the cutoff from the online set, and
-- return them (SES-B4).
--
-- Reading and removing in one script means that when several Session instances trim at
-- once, each player who went offline is returned to exactly one of them, so each friend
-- is told once.
--
-- KEYS[1] presence:online
-- ARGV[1] cutoff, unix milliseconds: a score below it is gone

local bound = '(' .. ARGV[1]
local gone = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', bound)
if #gone > 0 then
	redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', bound)
end
return gone
