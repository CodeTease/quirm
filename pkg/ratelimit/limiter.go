package ratelimit

import (
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/time/rate"
)

type Limiter interface {
	Allow(key string) bool
}

type MemoryLimiter struct {
	limiters *expirable.LRU[string, *rate.Limiter]
	r        rate.Limit
	b        int
	mu       sync.Mutex
}

func NewMemoryLimiter(requestsPerSecond int, size int, ttl time.Duration) *MemoryLimiter {
	return &MemoryLimiter{
		limiters: expirable.NewLRU[string, *rate.Limiter](size, nil, ttl),
		r:        rate.Limit(requestsPerSecond),
		b:        requestsPerSecond, // burst equals limit
	}
}

func (m *MemoryLimiter) Allow(key string) bool {
	// Get or create limiter
	limiter, exists := m.limiters.Get(key)
	if !exists {
		// Note: This has a race condition where we might create multiple limiters for the same key
		// if calls are concurrent, but it is acceptable for rate limiting.
		// Alternatively, we could lock.
		m.mu.Lock()
		// Double check
		limiter, exists = m.limiters.Get(key)
		if !exists {
			limiter = rate.NewLimiter(m.r, m.b)
			m.limiters.Add(key, limiter)
		}
		m.mu.Unlock()
	}
	return limiter.Allow()
}
