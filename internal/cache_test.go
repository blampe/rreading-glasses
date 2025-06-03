package internal

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCache(t *testing.T) {
	ctx := context.Background()
	c0 := newMemoryCache()
	c1 := newMemoryCache()

	l := &LayeredCache{wrapped: []cache[[]byte]{c0, c1}}

	t.Run("miss", func(t *testing.T) {
		out, ok := l.Get(ctx, "miss")
		assert.False(t, ok)
		assert.Nil(t, out)
	})

	t.Run("percolation", func(t *testing.T) {
		key := "c0-miss"
		val := []byte(key)

		// Only c1 starts with the entry,
		c1.Set(ctx, key, val, time.Hour)

		out, ok := l.Get(ctx, key)
		assert.True(t, ok)
		assert.Equal(t, val, out)

		// c0 now has it.
		out, ttl, ok := c0.GetWithTTL(ctx, key)
		assert.True(t, ok)
		assert.Equal(t, val, out)
		assert.Greater(t, ttl, time.Minute)
	})

	t.Run("set-get", func(t *testing.T) {
		key := "set-get"
		val := []byte(key)

		l.Set(ctx, key, val, time.Hour)

		out, ok := c0.Get(ctx, key)
		assert.True(t, ok)
		assert.Equal(t, val, out)

		out, ok = c1.Get(ctx, key)
		assert.True(t, ok)
		assert.Equal(t, val, out)

		out, ok = l.Get(ctx, key)
		assert.True(t, ok)
		assert.Equal(t, val, out)
	})
}
