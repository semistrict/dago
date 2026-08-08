// Package cache defines deterministic node and model result caching contracts.
package cache

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Entry is a cached byte envelope. Callers own serialization and versioning.
type Entry struct {
	Value     []byte
	ExpiresAt time.Time
}

// Cache is the runtime cache contract.
type Cache interface {
	Get(context.Context, string) ([]byte, bool, error)
	Set(context.Context, string, []byte, time.Duration) error
	Delete(context.Context, string) error
}

// Memory is a concurrency-safe in-memory cache with lazy expiration.
type Memory struct {
	mu      sync.RWMutex
	entries map[string]Entry
	now     func() time.Time
}

func NewMemory() *Memory { return &Memory{entries: map[string]Entry{}, now: time.Now} }

func (memory *Memory) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := validate(ctx, key); err != nil {
		return nil, false, err
	}
	memory.mu.RLock()
	entry, ok := memory.entries[key]
	memory.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	if !entry.ExpiresAt.IsZero() && !memory.now().Before(entry.ExpiresAt) {
		memory.mu.Lock()
		if current, exists := memory.entries[key]; exists && current.ExpiresAt.Equal(entry.ExpiresAt) {
			delete(memory.entries, key)
		}
		memory.mu.Unlock()
		return nil, false, nil
	}
	return append([]byte(nil), entry.Value...), true, nil
}

func (memory *Memory) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := validate(ctx, key); err != nil {
		return err
	}
	entry := Entry{Value: append([]byte(nil), value...)}
	if ttl > 0 {
		entry.ExpiresAt = memory.now().Add(ttl)
	}
	memory.mu.Lock()
	memory.entries[key] = entry
	memory.mu.Unlock()
	return nil
}

func (memory *Memory) Delete(ctx context.Context, key string) error {
	if err := validate(ctx, key); err != nil {
		return err
	}
	memory.mu.Lock()
	delete(memory.entries, key)
	memory.mu.Unlock()
	return nil
}

func validate(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("cache key is required")
	}
	return nil
}
