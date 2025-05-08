package internal

import (
	"context"
	"testing"
	"time"

	"github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCache(t *testing.T) {
	ctx := context.Background()
	c0 := newMemory()
	c1 := newMemory()

	l := &LayeredCache{wrapped: []cache.SetterCacheInterface[[]byte]{c0, c1}}

	t.Run("miss", func(t *testing.T) {
		out, ok := l.Get(ctx, "miss")
		assert.False(t, ok)
		assert.Nil(t, out)
	})

	t.Run("percolation", func(t *testing.T) {
		key := "c0-miss"
		val := []byte(key)

		// Only c1 starts with the entry,
		err := c1.Set(ctx, key, val, store.WithExpiration(time.Hour), store.WithSynchronousSet())
		require.NoError(t, err)

		out, ok := l.Get(ctx, key)
		assert.True(t, ok)
		assert.Equal(t, val, out)

		// c0 now has it.
		out, ttl, err := c0.GetWithTTL(ctx, key)
		assert.NoError(t, err)
		assert.Equal(t, val, out)
		assert.Greater(t, ttl, time.Minute)
	})

	t.Run("set-get", func(t *testing.T) {
		key := "set-get"
		val := []byte(key)

		l.Set(ctx, key, val, time.Hour)

		out, err := c0.Get(ctx, key)
		assert.NoError(t, err)
		assert.Equal(t, val, out)

		out, err = c1.Get(ctx, key)
		assert.NoError(t, err)
		assert.Equal(t, val, out)

		out, ok := l.Get(ctx, key)
		assert.True(t, ok)
		assert.Equal(t, val, out)
	})
}
