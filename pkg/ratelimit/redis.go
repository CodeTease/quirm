package ratelimit

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisLimiter struct {
	client redis.UniversalClient
	limit  int
	window time.Duration
}

func NewRedisLimiter(addrs []string, password string, db int, limit int) *RedisLimiter {
	// If only one address, we can check if it works as a single node
	// But UniversalClient handles single/cluster/sentinel logic based on options
	// If addrs has >1 item -> Cluster
	// If addrs has 1 item -> Single node

	rdb := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:    addrs,
		Password: password,
		DB:       db,
	})

	return &RedisLimiter{
		client: rdb,
		limit:  limit,
		window: time.Second, // Fixed window size for rate limit (e.g. N reqs / 1 sec)
	}
}

func (r *RedisLimiter) Allow(key string) bool {
	ctx := context.Background()
	now := time.Now()

	// Lua script
	script := `
		local key = KEYS[1]
		local limit = tonumber(ARGV[1])
		local now = tonumber(ARGV[2])
		local window_start = now - tonumber(ARGV[3])
		
		-- Remove old entries
		redis.call('ZREMRANGEBYSCORE', key, '-inf', window_start)
		
		-- Count current entries
		local count = redis.call('ZCARD', key)
		
		if count < limit then
			-- Add new entry. We use 'now' as both score and member.
			-- Note: If two requests have exact same microsecond timestamp, 
			-- they will be deduped (counted as 1). This is acceptable for rate limiting usually.
			redis.call('ZADD', key, now, now)
			redis.call('EXPIRE', key, tonumber(ARGV[4])) -- Expire key after window (plus buffer)
			return 1
		end
		
		return 0
	`

	// Provide arguments
	// now in microseconds to be precise enough?
	nowMicro := now.UnixMicro()
	windowMicro := r.window.Microseconds()
	expireSeconds := int(r.window.Seconds()) + 1

	val, err := r.client.Eval(ctx, script, []string{"ratelimit:" + key}, r.limit, nowMicro, windowMicro, expireSeconds).Int()

	if err != nil {
		// Fail open if Redis fails
		return true
	}

	return val == 1
}
