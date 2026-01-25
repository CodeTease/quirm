package ratelimit

import (
	"testing"
	"time"
)

func TestMemoryLimiter_Allow(t *testing.T) {
	// 5 reqs per second
	limiter := NewMemoryLimiter(5, 100, time.Hour)
	key := "test-ip"
	
	// Should allow 5 requests
	for i := 0; i < 5; i++ {
		if !limiter.Allow(key) {
			t.Errorf("Request %d should be allowed", i+1)
		}
	}
	
	// 6th request should fail
	if limiter.Allow(key) {
		t.Error("Request 6 should be rejected")
	}
}
