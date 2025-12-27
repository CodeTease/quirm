package cache

import (
	"context"
	"time"
)

// Ensure TieredCache implements CacheProvider
var _ CacheProvider = (*TieredCache)(nil)

type TieredCache struct {
	L1 CacheProvider // Memory
	L2 CacheProvider // Redis
}

func NewTieredCache(l1, l2 CacheProvider) *TieredCache {
	return &TieredCache{
		L1: l1,
		L2: l2,
	}
}

func (c *TieredCache) Get(ctx context.Context, key string) ([]byte, bool) {
	// Try L1
	if val, found := c.L1.Get(ctx, key); found {
		return val, true
	}

	// Try L2
	if c.L2 != nil {
		if val, found := c.L2.Get(ctx, key); found {
			// Populate L1 (TTL? We don't have explicit TTL here, relying on L1 defaults)
			// Ideally Set should take 0 for default, or we pass a standard TTL.
			// Since MemoryCache ignores TTL in our current impl (uses construction TTL), 0 is fine.
			c.L1.Set(ctx, key, val, 0)
			return val, true
		}
	}

	return nil, false
}

func (c *TieredCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	// Set L1
	_ = c.L1.Set(ctx, key, value, ttl)

	// Set L2
	if c.L2 != nil {
		return c.L2.Set(ctx, key, value, ttl)
	}

	return nil
}

func (c *TieredCache) Delete(ctx context.Context, key string) error {
	_ = c.L1.Delete(ctx, key)
	if c.L2 != nil {
		return c.L2.Delete(ctx, key)
	}
	return nil
}
