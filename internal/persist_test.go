package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersister(t *testing.T) {
	ctx := t.Context()

	dsn := "postgres://postgres@localhost:5432/test"

	p, err := NewPersister(ctx, dsn)
	require.NoError(t, err)

	authorIDs, err := p.Persisted(ctx)
	require.NoError(t, err)

	assert.Empty(t, authorIDs)

	assert.NoError(t, p.Persist(ctx, 2))
	assert.NoError(t, p.Persist(ctx, 1))
	assert.NoError(t, p.Persist(ctx, 1))

	authorIDs, err = p.Persisted(ctx)
	require.NoError(t, err)

	assert.ElementsMatch(t, []int64{1, 2}, authorIDs)

	assert.NoError(t, p.Delete(ctx, 1))
	assert.NoError(t, p.Delete(ctx, 2))
	assert.NoError(t, p.Delete(ctx, 10))
}
