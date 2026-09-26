-- Deletes the seat keys KEYS that hold the booking token ARGV[1]. Keys of other tokens are left alone, so one
-- request never frees the claim of another. Returns how many keys were deleted.
local deleted = 0
for _, key in ipairs(KEYS) do
  if redis.call('GET', key) == ARGV[1] then
    redis.call('DEL', key)
    deleted = deleted + 1
  end
end
return deleted
