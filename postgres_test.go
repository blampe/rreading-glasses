package main

import (
	"context"
	"testing"
	"time"

	"github.com/eko/gocache/lib/v4/store"
	_ "github.com/mattn/go-sqlite3" // In-memory driver for testing.

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
	assert.Nil(t, expired)

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
