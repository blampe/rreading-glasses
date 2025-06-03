package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

type cache[T any] interface {
	Get(ctx context.Context, key string) (T, bool)
	GetWithTTL(ctx context.Context, key string) (T, time.Duration, bool)
	Set(ctx context.Context, key string, value T, ttl time.Duration)
	Expire(ctx context.Context, key string) error
}

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

	wrapped []cache[[]byte]
}

var _ cache[[]byte] = (*LayeredCache)(nil)

// GetWithTTL returns the cached value and its TTL. The boolean returned is
// false if no value was found.
func (c *LayeredCache) GetWithTTL(ctx context.Context, key string) ([]byte, time.Duration, bool) {
	var val []byte
	var ttl time.Duration
	var ok bool

	for _, cc := range c.wrapped {
		val, ttl, ok = cc.GetWithTTL(ctx, key)
		if !ok {
			// Percolate the value back up if we eventually find it.
			defer func(cc cache[[]byte]) {
				if val == nil {
					return
				}
				cc.Set(ctx, key, val, ttl)
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
		err = errors.Join(cc.Expire(ctx, key))
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
	for _, cc := range c.wrapped {
		cc.Set(ctx, key, val, ttl)
	}
}

// NewCache constructs a new layered cache.
func NewCache(ctx context.Context, dsn string) (*LayeredCache, error) {
	m := newMemoryCache()
	pg, err := newPostgres(ctx, dsn)
	if err != nil {
		return nil, err
	}
	c := &LayeredCache{wrapped: []cache[[]byte]{m, pg}}

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
