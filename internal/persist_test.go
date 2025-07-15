package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersister(t *testing.T) {
	ctx := t.Context()

	dsn := "postgres://postgres@localhost:5432/test"
	cache, err := NewCache(t.Context(), dsn, nil)
	require.NoError(t, err)

	p, err := NewPersister(ctx, cache, dsn)
	require.NoError(t, err)

	authorIDs, err := p.Persisted(ctx)
	require.NoError(t, err)

	assert.Empty(t, authorIDs)

	assert.NoError(t, p.Persist(ctx, 2, _missing))
	assert.NoError(t, p.Persist(ctx, 1, _missing))
	assert.NoError(t, p.Persist(ctx, 1, _missing))

	authorIDs, err = p.Persisted(ctx)
	require.NoError(t, err)

	assert.ElementsMatch(t, []int64{1, 2}, authorIDs)

	assert.NoError(t, p.Delete(ctx, 1))
	assert.NoError(t, p.Delete(ctx, 2))
	assert.NoError(t, p.Delete(ctx, 10))
}
