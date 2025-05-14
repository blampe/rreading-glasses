package internal

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eko/gocache/lib/v4/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres(t *testing.T) {
	ctx := context.Background()

	cache, err := newPostgres(ctx, "postgres://postgres@localhost:5432/test")
	require.NoError(t, err)

	missing, err := cache.Get(ctx, "missing")
	assert.ErrorContains(t, err, store.NOT_FOUND_ERR)
	assert.Nil(t, missing)

	err = cache.Set(ctx, "expired", []byte{1}, store.WithExpiration(0))
	require.NoError(t, err)
	expired, err := cache.Get(ctx, "expired")
	assert.ErrorContains(t, err, store.NOT_FOUND_ERR)
	assert.Equal(t, []byte{1}, expired)

	err = cache.Set(ctx, "cached", []byte{2}, store.WithExpiration(time.Hour))
	require.NoError(t, err)
	cached, ttl, err := cache.GetWithTTL(ctx, "cached")
	assert.NoError(t, err)
	assert.Equal(t, []byte{2}, cached)
	assert.Greater(t, ttl, time.Minute)

	err = cache.Set(ctx, "cached", []byte{3}, store.WithExpiration(time.Hour))
	require.NoError(t, err)
	updated, err := cache.Get(ctx, "cached")
	assert.NoError(t, err)
	assert.Equal(t, []byte{3}, updated)

	assert.NoError(t, cache.Invalidate(ctx, store.WithInvalidateTags([]string{"cached"})))
	assert.NoError(t, cache.Delete(ctx, "cached"))
}

// TestPostgresCache randomly writes and reads values from the cache
// concurrently to confirm things like our buffer pooling work correctly under
// load.
func TestPostgresCache(t *testing.T) {
	t.Parallel()

	dsn := "postgres://postgres@localhost:5432/test"
	ctx := context.Background()
	cache, err := NewCache(ctx, dsn)
	require.NoError(t, err)

	n := 500
	wg := sync.WaitGroup{}

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()

			s := strings.Repeat(fmt.Sprint(i), i)
			sleep := time.Duration(rand.Float64() / 10.0 * float64(time.Second))
			time.Sleep(sleep)
			cache.Set(ctx, fmt.Sprint(i), []byte(s), time.Minute)
		}()
	}
	wg.Wait()

	checkCache := func(cache *LayeredCache) {
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()

				sleep := time.Duration(rand.Float64() / 10.0 * float64(time.Second))
				time.Sleep(sleep)
				actual, ok := cache.Get(ctx, fmt.Sprint(i))
				if i == 0 {
					// Empty value isn't set.
					require.False(t, ok)
					return
				}
				require.True(t, ok)
				expected := strings.Repeat(fmt.Sprint(i), i)
				assert.Equal(t, expected, string(actual))
			}()

		}
		wg.Wait()
	}

	t.Run("warm in-memory cache", func(t *testing.T) {
		t.Parallel()
		checkCache(cache)
	})

	t.Run("cold in-memory cache", func(t *testing.T) {
		t.Parallel()
		// Create a new cache.
		coldCache, err := NewCache(ctx, dsn)
		require.NoError(t, err)
		checkCache(coldCache)
	})

	t.Cleanup(func() {
		for i := range n {
			_ = cache.Expire(ctx, fmt.Sprint(i))
		}
	})
}
