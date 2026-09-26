-- Deletes the record under KEYS[1] if the owner ARGV[1] still owns it. Returns 1 if it did and 0 otherwise.
if redis.call('HGET', KEYS[1], 'owner') ~= ARGV[1] then
  return 0
end
redis.call('DEL', KEYS[1])
return 1
