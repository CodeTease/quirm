package cache

import (
	"context"
	"time"

	"github.com/dgraph-io/ristretto"
)

// Ensure MemoryCache implements CacheProvider
var _ CacheProvider = (*MemoryCache)(nil)

type MemoryCache struct {
	cache *ristretto.Cache
}

func NewMemoryCache(size int, limitBytes int64, defaultTTL time.Duration) *MemoryCache {
	var maxCost int64
	var numCounters int64
	
	// Determine configuration
	if limitBytes > 0 {
		// Capacity-based limit
		maxCost = limitBytes
		estimatedItems := limitBytes / 10240 
		if estimatedItems < 100 {
			estimatedItems = 100
		}
		numCounters = estimatedItems * 10
	} else {
		// Item count-based limit
		maxCost = int64(size)
		if maxCost <= 0 {
			maxCost = 100 // Fallback
		}
		numCounters = maxCost * 10
	}

	config := &ristretto.Config{
		NumCounters: numCounters,
		MaxCost:     maxCost,
		BufferItems: 64, // Number of keys per Get buffer.
		Metrics:     false,
	}

	// Cost function
	if limitBytes > 0 {
		config.Cost = func(value interface{}) int64 {
			if val, ok := value.([]byte); ok {
				return int64(len(val))
			}
			return 1
		}
	} else {
		// If using item count, cost is always 1
		config.Cost = func(value interface{}) int64 {
			return 1
		}
	}

	// We don't set IgnoreInternalCost: false, so overhead is not tracked automatically, which is fine.

	cache, err := ristretto.NewCache(config)
	if err != nil {
		panic(err)
	}

	return &MemoryCache{
		cache: cache,
	}
}

func (c *MemoryCache) Get(ctx context.Context, key string) ([]byte, bool) {
	val, found := c.cache.Get(key)
	if !found {
		return nil, false
	}
	if data, ok := val.([]byte); ok {
		return data, true
	}
	return nil, false
}

func (c *MemoryCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	// Pass 0 as cost to let Ristretto calculate it using the configured Cost function.
	c.cache.SetWithTTL(key, value, 0, ttl)
	return nil
}

func (c *MemoryCache) Delete(ctx context.Context, key string) error {
	c.cache.Del(key)
	return nil
}

func (c *MemoryCache) Health(ctx context.Context) error {
	return nil
}
