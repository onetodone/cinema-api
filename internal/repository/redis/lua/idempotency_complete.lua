-- Replaces the record under KEYS[1] with ARGV[2], which then expires after ARGV[3] milliseconds, if the owner
-- ARGV[1] still owns it. Returns 1 if it did and 0 otherwise.
if redis.call('HGET', KEYS[1], 'owner') ~= ARGV[1] then
  return 0
end
redis.call('HSET', KEYS[1], 'record', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
