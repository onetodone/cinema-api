-- Stores the record ARGV[2] for the owner ARGV[1] under KEYS[1], which expires after ARGV[3] milliseconds,
-- unless KEYS[1] exists. Returns nothing when it stored the record, and the stored record otherwise.
local existing = redis.call('HGET', KEYS[1], 'record')
if existing then
  return existing
end
redis.call('HSET', KEYS[1], 'owner', ARGV[1], 'record', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return false
