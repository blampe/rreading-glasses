//go:generate go run go.uber.org/mock/mockgen -typed -source controller.go -package internal -destination internal/mock.go . getter

package main

import (
	"context"
	"encoding/json"
	"iter"
	"testing"
	"time"

	"github.com/blampe/rreading-glasses/internal"
	"github.com/eko/gocache/lib/v4/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestIncrementalEnsure(t *testing.T) {
	// Looking up foreign editions should update relevant works to include
	// those editions, and authors should be updated to reflect the new works.
	t.Parallel()

	ctx := context.Background()
	c := gomock.NewController(t)
	getter := internal.NewMockgetter(c)

	work := workResource{ForeignID: 1}

	englishEdition := bookResource{ForeignID: 100, Language: "en"}
	frenchEdition := bookResource{ForeignID: 200, Language: "fr"}
	work.Books = []bookResource{englishEdition}

	author := authorResource{ForeignID: 1000, Works: []workResource{work}}

	work.Authors = []authorResource{author}

	initialAuthorBytes, err := json.Marshal(author)
	require.NoError(t, err)
	initialWorkBytes, err := json.Marshal(work)
	require.NoError(t, err)
	frenchEditionBytes, err := json.Marshal(workResource{ForeignID: work.ForeignID, Books: []bookResource{frenchEdition}})
	require.NoError(t, err)
	englishEditionBytes, err := json.Marshal(workResource{ForeignID: work.ForeignID, Books: []bookResource{englishEdition}})
	require.NoError(t, err)

	cache := &layeredcache{wrapped: []cache.SetterCacheInterface[[]byte]{newMemory()}}

	ctrl, err := newController(cache, getter)
	require.NoError(t, err)

	go func() {
		ctrl.Run(ctx)
	}()

	// TODO: Generalize this into a test helper.
	getter.EXPECT().GetAuthor(gomock.Any(), author.ForeignID).DoAndReturn(func(ctx context.Context, authorID int64) ([]byte, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, authorKey(authorID))
		if ok {
			return cachedBytes, nil
		}
		return initialAuthorBytes, nil
	}).AnyTimes()

	getter.EXPECT().GetBook(gomock.Any(), englishEdition.ForeignID).DoAndReturn(func(ctx context.Context, bookID int64) ([]byte, int64, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, bookKey(bookID))
		if ok {
			return cachedBytes, 0, 0, nil
		}
		return englishEditionBytes, work.ForeignID, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetBook(gomock.Any(), frenchEdition.ForeignID).DoAndReturn(func(ctx context.Context, bookID int64) ([]byte, int64, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, bookKey(bookID))
		if ok {
			return cachedBytes, 0, 0, nil
		}
		return frenchEditionBytes, work.ForeignID, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), work.ForeignID).DoAndReturn(func(ctx context.Context, workID int64) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, workKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkBytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetAuthorBooks(gomock.Any(), author.ForeignID).Return(
		func(yield func(int64) bool) {
			if !yield(englishEdition.ForeignID) {
				return
			}
			if !yield(frenchEdition.ForeignID) {
				return
			}
		},
	).AnyTimes()

	// Getting the author will initially return it with only the "best" original-language edition.
	authorBytes, err := ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)

	require.NoError(t, json.Unmarshal(authorBytes, &author))

	assert.Len(t, author.Works, 1)
	assert.Equal(t, englishEdition.ForeignID, author.Works[0].Books[0].ForeignID)

	// Getting a foreign edition should add it to the work.
	_, err = ctrl.GetBook(ctx, frenchEdition.ForeignID)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond) // Wait for the ensure goroutine update things.

	workBytes, err := ctrl.GetWork(ctx, work.ForeignID)
	require.NoError(t, err)
	var w workResource
	require.NoError(t, json.Unmarshal(workBytes, &w))
	assert.Len(t, w.Books, 2)

	// The work should have also been updated on the author.
	authorBytes, err = ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(authorBytes, &author))
	assert.Len(t, author.Works, 1)
	assert.Len(t, author.Works[0].Books, 2)
	assert.Equal(t, englishEdition.ForeignID, author.Works[0].Books[0].ForeignID)
	assert.Equal(t, frenchEdition.ForeignID, author.Works[0].Books[1].ForeignID)

	// Force a cache miss to re-trigger ensure.
	_ = ctrl.cache.Delete(ctx, bookKey(frenchEdition.ForeignID))
	_, _ = ctrl.GetBook(ctx, frenchEdition.ForeignID)

	time.Sleep(100 * time.Millisecond) // Wait for the ensure goroutine update things.

	workBytes, err = ctrl.GetWork(ctx, work.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(workBytes, &w))
	assert.Len(t, w.Books, 2)

	authorBytes, err = ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(authorBytes, &author))
	assert.Len(t, author.Works[0].Books, 2)

	// Force an author cache miss to re-trigger ensure.
	_ = ctrl.cache.Delete(ctx, authorKey(author.ForeignID))
	_, _ = ctrl.GetAuthor(ctx, author.ForeignID)

	time.Sleep(100 * time.Millisecond) // Wait for the ensure goroutine update things.

	authorBytes, err = ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(authorBytes, &author))
	assert.Len(t, author.Works[0].Books, 2)
}

func TestEnsureMissing(t *testing.T) {
	// Ensuring relationships on objects that are missing should no-op.
	ctx := context.Background()

	authorID := int64(1)
	workID := int64(2)
	bookID := int64(3)

	cache := &layeredcache{wrapped: []cache.SetterCacheInterface[[]byte]{newMemory()}}

	notFoundGetter := internal.NewMockgetter(gomock.NewController(t))
	notFoundGetter.EXPECT().GetAuthor(gomock.Any(), authorID).Return(nil, errNotFound).AnyTimes()
	notFoundGetter.EXPECT().GetWork(gomock.Any(), workID).Return(nil, 0, errNotFound).AnyTimes()

	ctrl, err := newController(cache, notFoundGetter)
	require.NoError(t, err)

	err = ctrl.ensureEditions(ctx, workID, bookID)
	assert.ErrorIs(t, err, errNotFound)

	err = ctrl.ensureWorks(ctx, authorID, workID)
	assert.ErrorIs(t, err, errNotFound)
}

func TestSubtitles(t *testing.T) {
	// Subtitles (i.e. FullTitle) are used in situations where multiple works share the same primary title.

	t.Parallel()

	ctx := context.Background()
	c := gomock.NewController(t)
	getter := internal.NewMockgetter(c)

	workDupe1 := workResource{
		ForeignID: 1,
		Title:     "Foo",
		FullTitle: "Foo: First Work",
		Books: []bookResource{
			{ForeignID: 1, Title: "Foo", FullTitle: "Foo: First Edition"},
			{ForeignID: 2, Title: "Foo", FullTitle: ""},
		},
	}

	workDupe2 := workResource{
		ForeignID: 2,
		Title:     "Foo",
		FullTitle: "Foo: Second Work",
		Books: []bookResource{
			{ForeignID: 10, Title: "Foo", FullTitle: "Foo: Second Edition"},
			{ForeignID: 20, Title: "Foo", FullTitle: ""},
		},
	}

	workUnique := workResource{
		ForeignID: 3,
		Title:     "Bar",
		FullTitle: "Bar: Not Foo",
		Books: []bookResource{
			{ForeignID: 10, Title: "Bar", FullTitle: "Bar: Not Foo"},
			{ForeignID: 20, Title: "Bar", FullTitle: ""},
		},
	}

	author := authorResource{ForeignID: 1000, Works: []workResource{workDupe1, workDupe2, workUnique}}

	workDupe1.Authors = []authorResource{author}
	workDupe2.Authors = []authorResource{author}
	workUnique.Authors = []authorResource{author}

	initialAuthorBytes, err := json.Marshal(author)
	require.NoError(t, err)
	initialWorkDupe1Bytes, err := json.Marshal(workDupe1)
	require.NoError(t, err)
	initialWorkDupe2Bytes, err := json.Marshal(workDupe2)
	require.NoError(t, err)
	initialWorkUniqueBytes, err := json.Marshal(workUnique)
	require.NoError(t, err)

	cache := &layeredcache{wrapped: []cache.SetterCacheInterface[[]byte]{newMemory()}}

	ctrl, err := newController(cache, getter)
	require.NoError(t, err)

	getter.EXPECT().GetAuthor(gomock.Any(), author.ForeignID).DoAndReturn(func(ctx context.Context, authorID int64) ([]byte, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, authorKey(authorID))
		if ok {
			return cachedBytes, nil
		}
		return initialAuthorBytes, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workDupe1.ForeignID).DoAndReturn(func(ctx context.Context, workID int64) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, workKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkDupe1Bytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workDupe2.ForeignID).DoAndReturn(func(ctx context.Context, workID int64) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, workKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkDupe2Bytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workUnique.ForeignID).DoAndReturn(func(ctx context.Context, workID int64) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, workKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkUniqueBytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetAuthorBooks(gomock.Any(), author.ForeignID).Return(iter.Seq[int64](func(func(int64) bool) {}))

	err = ctrl.ensureWorks(ctx, author.ForeignID, workDupe1.ForeignID, workDupe2.ForeignID, workUnique.ForeignID)
	require.NoError(t, err)

	authorBytes, err := ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)

	require.NoError(t, json.Unmarshal(authorBytes, &author))

	assert.Equal(t, "Foo: First Work", author.Works[0].Title)
	assert.Equal(t, "Foo: Second Work", author.Works[1].Title)
	assert.Equal(t, "Bar", author.Works[2].Title)

	assert.Equal(t, "Foo: First Edition", author.Works[0].Books[0].Title)
	assert.Equal(t, "Foo", author.Works[0].Books[1].Title)

	assert.Equal(t, "Foo: Second Edition", author.Works[1].Books[0].Title)
	assert.Equal(t, "Foo", author.Works[1].Books[1].Title)

	assert.Equal(t, "Bar", author.Works[2].Books[0].Title)
	assert.Equal(t, "Bar", author.Works[2].Books[1].Title)
}
