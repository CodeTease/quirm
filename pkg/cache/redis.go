package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ensure RedisCache implements CacheProvider
var _ CacheProvider = (*RedisCache)(nil)

type RedisCache struct {
	client redis.UniversalClient
}

func NewRedisCache(addrs []string, password string, db int) *RedisCache {
	return &RedisCache{
		client: redis.NewUniversalClient(&redis.UniversalOptions{
			Addrs:    addrs,
			Password: password,
			DB:       db,
		}),
	}
}

func (c *RedisCache) Get(ctx context.Context, key string) ([]byte, bool) {
	val, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	return val, true
}

func (c *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}

func (c *RedisCache) Delete(ctx context.Context, key string) error {
	return c.client.Del(ctx, key).Err()
}

func (c *RedisCache) Health(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}
