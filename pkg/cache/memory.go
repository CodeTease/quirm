package cache

import (
	"context"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// Ensure MemoryCache implements CacheProvider
var _ CacheProvider = (*MemoryCache)(nil)

type MemoryCache struct {
	cache *expirable.LRU[string, []byte]
}

func NewMemoryCache(size int, ttl time.Duration) *MemoryCache {
	return &MemoryCache{
		cache: expirable.NewLRU[string, []byte](size, nil, ttl),
	}
}

func (c *MemoryCache) Get(ctx context.Context, key string) ([]byte, bool) {
	return c.cache.Get(key)
}

func (c *MemoryCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	// Note: The underlying LRU has a fixed TTL set at creation time.
	// Overriding it per-item is not supported by expirable.LRU in simple mode easily,
	// unless we recreate the item or use a different library.
	// For now, we ignore the passed TTL and use the global one, or we just rely on LRU eviction.
	// However, expirable.LRU DOES check expiration on access.
	// Ideally we should use the passed TTL.
	// But `c.cache.Add` doesn't take TTL.
	c.cache.Add(key, value)
	return nil
}

func (c *MemoryCache) Delete(ctx context.Context, key string) error {
	c.cache.Remove(key)
	return nil
}
