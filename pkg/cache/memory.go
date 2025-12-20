package cache

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

type MemoryCache struct {
	cache *expirable.LRU[string, []byte]
}

func NewMemoryCache(size int, ttl time.Duration) *MemoryCache {
	return &MemoryCache{
		cache: expirable.NewLRU[string, []byte](size, nil, ttl),
	}
}

func (c *MemoryCache) Get(key string) ([]byte, bool) {
	return c.cache.Get(key)
}

func (c *MemoryCache) Set(key string, value []byte) {
	c.cache.Add(key, value)
}
