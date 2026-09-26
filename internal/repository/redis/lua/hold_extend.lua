-- Makes the seat keys KEYS that hold the booking token ARGV[1] expire at the Unix time ARGV[2], in
-- milliseconds. Keys of other tokens are left alone. Returns how many keys changed.
local changed = 0
for _, key in ipairs(KEYS) do
  if redis.call('GET', key) == ARGV[1] then
    redis.call('PEXPIREAT', key, ARGV[2])
    changed = changed + 1
  end
end
return changed
