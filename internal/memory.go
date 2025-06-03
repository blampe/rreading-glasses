package internal

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/dgraph-io/ristretto/v2"
)

var _ cache[[]byte] = (*memoryCache)(nil)

// newMemoryCache returns a new in-memory cache.
func newMemoryCache() cache[[]byte] {
	r, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 5e7,                                // Track LRU for up to 50M keys.
		MaxCost:     3 * (debug.SetMemoryLimit(-1) / 4), // Use 75% of available memory.
		BufferItems: 64,                                 // Number of keys per Get buffer.
	})
	if err != nil {
		panic(err)
	}

	return &memoryCache{r}
}

type memoryCache struct {
	r *ristretto.Cache[string, []byte]
}

func (c *memoryCache) Get(_ context.Context, key string) ([]byte, bool) {
	return c.r.Get(key)
}

func (c *memoryCache) GetWithTTL(ctx context.Context, key string) ([]byte, time.Duration, bool) {
	ttl, ok := c.r.GetTTL(key)
	bytes, _ := c.Get(ctx, key)
	return bytes, ttl, ok
}

func (c *memoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) {
	_ = c.r.SetWithTTL(key, value, int64(len(value)), ttl)
	c.r.Wait() // Synchronous set.
}

func (c *memoryCache) Expire(_ context.Context, key string) error {
	c.r.Del(key)
	return nil
}
