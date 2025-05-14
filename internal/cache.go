package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/store"
	memory "github.com/eko/gocache/store/ristretto/v4"
)

// LayeredCache implements a simple tiered cache. In practice we use an
// in-memory cache backed by Postgres for persistent storage. Hits at lower
// layers are automatically percolated up. Values are compressed with gzip at
// rest.
//
// cache.ChainCache has inconsistent marshaling behavior, so we use our own
// wrapper. Actually that package doesn't really buy us anything...
type LayeredCache struct {
	hits   atomic.Int64
	misses atomic.Int64

	wrapped []cache.SetterCacheInterface[[]byte]
}

// GetWithTTL returns the cached value and its TTL. The boolean returned is
// false if no value was found.
func (c *LayeredCache) GetWithTTL(ctx context.Context, key string) ([]byte, time.Duration, bool) {
	var val []byte
	var ttl time.Duration
	var err error

	for idx, cc := range c.wrapped {
		val, ttl, err = cc.GetWithTTL(ctx, key)
		if err != nil {
			// Percolate the value back up if we eventually find it.
			defer func(cc cache.SetterCacheInterface[[]byte]) {
				if val == nil {
					return
				}
				err = cc.Set(ctx, key, val, store.WithExpiration(ttl), store.WithSynchronousSet())
				if err != nil {
					Log(ctx).Warn("problem caching", "key", key, "layer", idx)
				}
			}(cc)
			continue
		}

		_ = c.hits.Add(1)

		return val, ttl, true
	}

	_ = c.misses.Add(1)

	return nil, 0, false
}

// Get returns a cache value, if it exists, and a boolean if a value was found.
func (c *LayeredCache) Get(ctx context.Context, key string) ([]byte, bool) {
	val, _, ok := c.GetWithTTL(ctx, key)
	return val, ok
}

// Expire expires a key from all layers of the cache. This removes it from
// memory but keeps data persisted in Postgres without a TTL.
func (c *LayeredCache) Expire(ctx context.Context, key string) error {
	var err error
	for _, cc := range c.wrapped {
		err = errors.Join(cc.Delete(ctx, key))
	}
	return err
}

// Set a key/value in all layers of the cache.
// TODO: Fuzz expiration
func (c *LayeredCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) {
	if len(val) == 0 {
		Log(ctx).Warn("refusing to set empty value", "key", key)
		return
	}
	if ttl == 0 {
		Log(ctx).Warn("refusing to set zero ttl", "key", key)
		return
	}

	// TODO: We can offload the DB write to a background goroutine to speed
	// things up.
	for idx, cc := range c.wrapped {
		err := cc.Set(ctx, key, val, store.WithExpiration(ttl), store.WithSynchronousSet())
		if err != nil {
			Log(ctx).Warn("problem setting cache", "err", err, "layer", idx)
		}
	}
}

// NewCache constructs a new layered cache.
func NewCache(ctx context.Context, dsn string) (*LayeredCache, error) {
	m := newMemory()
	pg, err := newPostgres(ctx, dsn)
	if err != nil {
		return nil, err
	}
	c := &LayeredCache{wrapped: []cache.SetterCacheInterface[[]byte]{m, pg}}

	// Log cache stats every minute.
	go func() {
		for {
			time.Sleep(1 * time.Minute)
			hits, misses := c.hits.Load(), c.misses.Load()
			Log(ctx).LogAttrs(ctx, slog.LevelDebug, "cache stats",
				slog.Int64("hits", hits),
				slog.Int64("misses", misses),
				slog.Float64("ratio", float64(hits)/(float64(hits)+float64(misses))),
			)
		}
	}()

	return c, nil
}

func newMemory() *cache.Cache[[]byte] {
	r, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 5e7,                                // Track LRU for up to 50M keys.
		MaxCost:     3 * (debug.SetMemoryLimit(-1) / 4), // Use 75% of available memory.
		BufferItems: 64,                                 // Number of keys per Get buffer.
	})
	if err != nil {
		panic(err)
	}

	store := memory.NewRistretto(r)
	return cache.New[[]byte](store)
}

// WorkKey returns a cache key for a work ID.
func WorkKey(workID int64) string {
	return fmt.Sprintf("w%d", workID)
}

// BookKey returns a cache key for a book (edition) ID.
func BookKey(bookID int64) string {
	return fmt.Sprintf("b%d", bookID)
}

// AuthorKey returns a cache key for an author ID.
func AuthorKey(authorID int64) string {
	return fmt.Sprintf("a%d", authorID)
}
