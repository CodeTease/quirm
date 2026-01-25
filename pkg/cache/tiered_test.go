package cache

import (
	"context"
	"testing"
	"time"
)

// MockCacheProvider
type MockCacheProvider struct {
	GetFunc func(ctx context.Context, key string) ([]byte, bool)
	SetFunc func(ctx context.Context, key string, value []byte, ttl time.Duration) error
	DeleteFunc func(ctx context.Context, key string) error
	HealthFunc func(ctx context.Context) error
}

func (m *MockCacheProvider) Get(ctx context.Context, key string) ([]byte, bool) {
	if m.GetFunc != nil {
		return m.GetFunc(ctx, key)
	}
	return nil, false
}

func (m *MockCacheProvider) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if m.SetFunc != nil {
		return m.SetFunc(ctx, key, value, ttl)
	}
	return nil
}

func (m *MockCacheProvider) Delete(ctx context.Context, key string) error {
	if m.DeleteFunc != nil {
		return m.DeleteFunc(ctx, key)
	}
	return nil
}

func (m *MockCacheProvider) Health(ctx context.Context) error {
	if m.HealthFunc != nil {
		return m.HealthFunc(ctx)
	}
	return nil
}

func TestTieredCache_Get_HitL1(t *testing.T) {
	ctx := context.Background()
	key := "test-key"
	val := []byte("value")
	
	l1 := &MockCacheProvider{
		GetFunc: func(ctx context.Context, k string) ([]byte, bool) {
			if k == key {
				return val, true
			}
			return nil, false
		},
	}
	l2 := &MockCacheProvider{
		GetFunc: func(ctx context.Context, k string) ([]byte, bool) {
			t.Error("L2 should not be called")
			return nil, false
		},
	}
	
	c := NewTieredCache(l1, l2)
	
	got, found := c.Get(ctx, key)
	if !found {
		t.Error("Expected found")
	}
	if string(got) != string(val) {
		t.Errorf("Expected %s, got %s", val, got)
	}
}

func TestTieredCache_Get_HitL2(t *testing.T) {
	ctx := context.Background()
	key := "test-key"
	val := []byte("value")
	
	l1SetCalled := false
	l1 := &MockCacheProvider{
		GetFunc: func(ctx context.Context, k string) ([]byte, bool) {
			return nil, false
		},
		SetFunc: func(ctx context.Context, k string, v []byte, ttl time.Duration) error {
			if k == key && string(v) == string(val) {
				l1SetCalled = true
			}
			return nil
		},
	}
	l2 := &MockCacheProvider{
		GetFunc: func(ctx context.Context, k string) ([]byte, bool) {
			if k == key {
				return val, true
			}
			return nil, false
		},
	}
	
	c := NewTieredCache(l1, l2)
	
	got, found := c.Get(ctx, key)
	if !found {
		t.Error("Expected found")
	}
	if string(got) != string(val) {
		t.Errorf("Expected %s, got %s", val, got)
	}
	if !l1SetCalled {
		t.Error("Expected L1 Set to be called")
	}
}

func TestTieredCache_Get_MissAll(t *testing.T) {
	ctx := context.Background()
	key := "test-key"
	
	l1 := &MockCacheProvider{
		GetFunc: func(ctx context.Context, k string) ([]byte, bool) {
			return nil, false
		},
	}
	l2 := &MockCacheProvider{
		GetFunc: func(ctx context.Context, k string) ([]byte, bool) {
			return nil, false
		},
	}
	
	c := NewTieredCache(l1, l2)
	
	_, found := c.Get(ctx, key)
	if found {
		t.Error("Expected not found")
	}
}
