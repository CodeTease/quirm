package ratelimit

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisLimiter struct {
	client *redis.Client
	limit  int
	window time.Duration
}

func NewRedisLimiter(addr, password string, db int, limit int) *RedisLimiter {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	return &RedisLimiter{
		client: rdb,
		limit:  limit,
		window: time.Second, // Fixed window of 1 second
	}
}

func (r *RedisLimiter) Allow(key string) bool {
	ctx := context.Background()
	// Simple fixed window: key is "ratelimit:<key>:<current_timestamp_seconds>"
	// Or just "ratelimit:<key>" with expiration if we use a sliding window via Lua script,
	// but fixed window is requested/simple.
	
	// Better approach for distributed limit:
	// INCR key
	// If result == 1, EXPIRE key 1s
	// If result > limit, return false
	
	// Use a key that expires every second? No, we need it to expire *after* 1 second from creation.
	
	rateKey := "ratelimit:" + key + ":" + time.Now().Format("2006-01-02T15:04:05")
	
	// Pipelined for atomicity not strictly required for this logic but good for performance
	pipe := r.client.Pipeline()
	incr := pipe.Incr(ctx, rateKey)
	pipe.Expire(ctx, rateKey, 2*time.Second) // Give it a bit more time to be safe, exact expiry handled by key name change
	_, err := pipe.Exec(ctx)
	
	if err != nil {
		// Fail open or closed? Fail open (allow) is usually safer for rate limits
		return true
	}
	
	count := incr.Val()
	return count <= int64(r.limit)
}
