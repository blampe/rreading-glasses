package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/store"
	memory "github.com/eko/gocache/store/ristretto/v4"
	"github.com/klauspost/compress/gzip"
)

// layeredcache implements a simple tiered cache. In practice we use an
// in-memory cache backed by Postgres for persistent storage. Hits at lower
// layers are automatically percolated up. Values are compressed with gzip at
// rest.
//
// cache.ChainCache has inconsistent marshaling behavior, so we use our own
// wrapper. Actually that package doesn't really buy us anything...
type layeredcache struct {
	wrapped []cache.SetterCacheInterface[[]byte]
}

func (c *layeredcache) GetWithTTL(ctx context.Context, key string) ([]byte, time.Duration, bool) {
	var compressed []byte
	var ttl time.Duration
	var err error

	for idx, cc := range c.wrapped {
		compressed, ttl, err = cc.GetWithTTL(ctx, key)
		if err != nil {
			// Percolate the value back up if we eventually find it.
			defer func(cc cache.SetterCacheInterface[[]byte]) {
				if compressed == nil {
					return
				}
				log(ctx).Debug("percolating", "key", key, "size", len(compressed), "ttl", ttl)
				err = cc.Set(ctx, key, compressed, store.WithExpiration(ttl), store.WithSynchronousSet())
				if err != nil {
					log(ctx).Warn("problem caching", "key", key, "layer", idx)
				}
			}(cc)
			continue
		}

		zr, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil && !errors.Is(err, io.EOF) {
			log(ctx).Warn("problem unzipping", "err", err)
			// Ignore this cached value and try the next.
			compressed = nil
			continue
		}

		// TODO: The client doesn't support gzip content-encoding, which is too
		// bade because we could just return compressed bytes as-is.
		uncompressed := bytes.Buffer{}

		_, err = io.Copy(&uncompressed, zr)
		if err != nil && !errors.Is(err, io.EOF) {
			log(ctx).Warn("problem decompressing", "err", err)
		}
		if err := zr.Close(); err != nil {
			log(ctx).Warn("problem closing zip write", "err", err)
		}

		log(ctx).LogAttrs(ctx, slog.LevelDebug, "cache hit",
			slog.Int("layer", idx),
			slog.String("key", key),
			slog.Int("size", len(compressed)),
			slog.Duration("ttl", ttl),
		)
		return uncompressed.Bytes(), ttl, true
	}

	log(ctx).LogAttrs(ctx, slog.LevelDebug, "cache miss",
		slog.String("key", key),
	)

	return nil, 0, false
}

func (c *layeredcache) Get(ctx context.Context, key string) ([]byte, bool) {
	val, _, ok := c.GetWithTTL(ctx, key)
	return val, ok
}

func (c *layeredcache) Delete(ctx context.Context, key string) error {
	var err error
	for _, cc := range c.wrapped {
		err = errors.Join(cc.Delete(ctx, key))
	}
	return err
}

// TODO: Fuzz expiration?
func (c *layeredcache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) {
	if len(val) == 0 {
		log(ctx).Warn("refusing to set empty value", "key", key)
		return
	}
	if ttl == 0 {
		log(ctx).Warn("refusing to set zero ttl", "key", key)
		return
	}

	compressed, err := compress(val)
	if err != nil && !errors.Is(err, io.EOF) {
		log(ctx).Warn("problem compressing", "err", err)
		return
	}

	log(ctx).Debug("setting cache", "key", key, "size", len(compressed), "ttl", ttl)
	// TODO: We can offload the DB write to a background goroutine to speed
	// things up.
	for idx, cc := range c.wrapped {
		err := cc.Set(ctx, key, compressed, store.WithExpiration(ttl), store.WithSynchronousSet())
		if err != nil {
			log(ctx).Warn("problem setting cache", "err", err, "layer", idx)
		}
	}
}

func compress(val []byte) ([]byte, error) {
	compressed := bytes.Buffer{}
	zw := gzip.NewWriter(&compressed)
	_, err := zw.Write(val)
	err = errors.Join(err, zw.Close())
	return compressed.Bytes(), err
}

func newCache(ctx context.Context, dsn string) (*layeredcache, error) {
	m := newMemory()
	pg, err := newPostgres(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &layeredcache{wrapped: []cache.SetterCacheInterface[[]byte]{m, pg}}, nil
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

func workKey(workID int64) string {
	return fmt.Sprintf("w%d", workID)
}

func bookKey(bookID int64) string {
	return fmt.Sprintf("b%d", bookID)
}

func authorKey(authorID int64) string {
	return fmt.Sprintf("a%d", authorID)
}
