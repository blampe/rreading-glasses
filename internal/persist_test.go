package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersister(t *testing.T) {
	ctx := t.Context()

	dsn := "postgres://postgres@localhost:5432/test"
	cache, err := NewCache(t.Context(), dsn, nil, nil)
	require.NoError(t, err)

	p, err := NewPersister(ctx, cache, dsn)
	require.NoError(t, err)

	authorIDs, err := p.Persisted(ctx)
	require.NoError(t, err)

	assert.Empty(t, authorIDs)

	assert.NoError(t, p.Persist(ctx, 2, _missing))
	assert.NoError(t, p.Persist(ctx, 1, _missing))
	assert.NoError(t, p.Persist(ctx, 1, _missing))
	assert.NoError(t, p.Persist(ctx, 3, _missing))

	authorIDs, err = p.Persisted(ctx)
	require.NoError(t, err)

	assert.Equal(t, []int64{2, 1, 3}, authorIDs)

	assert.NoError(t, p.Delete(ctx, 1))
	assert.NoError(t, p.Delete(ctx, 2))
	assert.NoError(t, p.Delete(ctx, 3))
	assert.NoError(t, p.Delete(ctx, 10))
}
