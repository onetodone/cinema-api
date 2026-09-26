-- Claims the seat keys KEYS for the booking token ARGV[1] for ARGV[2] milliseconds, all or nothing.
-- Returns the 1-based positions of the keys that another token holds, and claims nothing in that case. A key
-- that already holds ARGV[1] counts as free, so a repeated claim of the same booking succeeds.
local conflicts = {}
for i, key in ipairs(KEYS) do
  local owner = redis.call('GET', key)
  if owner and owner ~= ARGV[1] then
    conflicts[#conflicts + 1] = i
  end
end
if #conflicts > 0 then
  return conflicts
end
for _, key in ipairs(KEYS) do
  redis.call('SET', key, ARGV[1], 'PX', ARGV[2])
end
return {}
