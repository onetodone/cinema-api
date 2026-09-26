-- Counts one attempt in the fixed window KEYS[1], which lasts ARGV[1] milliseconds from its first attempt.
-- Returns the attempts in the window so far, this one included, and the milliseconds until the window ends.
local attempts = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[1], 'NX')
return {attempts, redis.call('PTTL', KEYS[1])}
