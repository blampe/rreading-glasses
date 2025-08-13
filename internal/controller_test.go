//go:generate go run go.uber.org/mock/mockgen -typed -source controller.go -package internal -destination mock.go . getter

package internal

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestIncrementalDenormalization(t *testing.T) {
	// Looking up foreign editions should update relevant works to include
	// those editions, and authors should be updated to reflect the new works.
	t.Parallel()

	ctx := context.Background()
	c := gomock.NewController(t)
	getter := NewMockgetter(c)

	work := workResource{ForeignID: 1}

	englishEdition := bookResource{ForeignID: 100, Language: "en"}
	frenchEdition := bookResource{ForeignID: 200, Language: "fr"}
	work.Books = []bookResource{englishEdition}

	authorID := int64(1000)
	author := AuthorResource{ForeignID: authorID, Works: []workResource{work}}

	work.Authors = []AuthorResource{author}

	initialAuthorBytes, err := json.Marshal(author)
	require.NoError(t, err)
	initialWorkBytes, err := json.Marshal(work)
	require.NoError(t, err)
	frenchEditionBytes, err := json.Marshal(workResource{ForeignID: work.ForeignID, Books: []bookResource{frenchEdition}})
	require.NoError(t, err)
	englishEditionBytes, err := json.Marshal(workResource{ForeignID: work.ForeignID, Books: []bookResource{englishEdition}})
	require.NoError(t, err)

	cache := newMemoryCache()

	ctrl, err := NewController(cache, getter, nil)
	require.NoError(t, err)

	go ctrl.Run(t.Context(), time.Millisecond)
	t.Cleanup(func() { ctrl.Shutdown(t.Context()) })

	// TODO: Generalize this into a test helper.
	getter.EXPECT().GetAuthor(gomock.Any(), author.ForeignID).DoAndReturn(func(ctx context.Context, authorID int64) ([]byte, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, AuthorKey(authorID))
		if ok {
			return cachedBytes, nil
		}
		return initialAuthorBytes, nil
	}).AnyTimes()

	getter.EXPECT().GetBook(gomock.Any(), englishEdition.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, bookID int64, saveEditions editionsCallback) ([]byte, int64, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, BookKey(bookID))
		if ok {
			return cachedBytes, 0, 0, nil
		}
		return englishEditionBytes, work.ForeignID, authorID, nil
	}).AnyTimes()

	getter.EXPECT().GetBook(gomock.Any(), frenchEdition.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, bookID int64, saveEditions editionsCallback) ([]byte, int64, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, BookKey(bookID))
		if ok {
			return cachedBytes, 0, 0, nil
		}
		return frenchEditionBytes, work.ForeignID, authorID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), work.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkBytes, authorID, nil
	}).AnyTimes()

	getter.EXPECT().GetAuthorBooks(gomock.Any(), authorID).Return(
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
	authorBytes, _, err := ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)

	require.NoError(t, json.Unmarshal(authorBytes, &author))

	assert.Len(t, author.Works, 1)
	assert.Equal(t, englishEdition.ForeignID, author.Works[0].Books[0].ForeignID)

	// Getting a foreign edition should add it to the work.
	_, _, err = ctrl.GetBook(ctx, frenchEdition.ForeignID)
	require.NoError(t, err)

	waitForDenorm(ctrl)

	workBytes, _, err := ctrl.GetWork(ctx, work.ForeignID)
	require.NoError(t, err)
	var w workResource
	require.NoError(t, json.Unmarshal(workBytes, &w))
	assert.Len(t, w.Books, 2)

	waitForDenorm(ctrl)

	// The work should have also been updated on the author.
	authorBytes, _, err = ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(authorBytes, &author))
	assert.Len(t, author.Works, 1)
	require.Len(t, author.Works[0].Books, 2)
	assert.Equal(t, englishEdition.ForeignID, author.Works[0].Books[0].ForeignID)
	assert.Equal(t, frenchEdition.ForeignID, author.Works[0].Books[1].ForeignID)

	// Force a cache miss to re-trigger denormalization.
	_ = ctrl.cache.Expire(ctx, BookKey(frenchEdition.ForeignID))
	_, _, _ = ctrl.GetBook(ctx, frenchEdition.ForeignID)

	_ = ctrl.refreshG.Wait()
	time.Sleep(100 * time.Millisecond) // Wait for the denormalization goroutine update things.

	workBytes, _, err = ctrl.GetWork(ctx, work.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(workBytes, &w))
	assert.Len(t, w.Books, 2)

	authorBytes, _, err = ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(authorBytes, &author))
	assert.Len(t, author.Works[0].Books, 2)

	// Force an author cache miss to re-trigger denormalization.
	_ = ctrl.cache.Expire(ctx, AuthorKey(author.ForeignID))
	_, _, _ = ctrl.GetAuthor(ctx, author.ForeignID)

	waitForDenorm(ctrl)

	authorBytes, _, err = ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(authorBytes, &author))
	assert.Len(t, author.Works[0].Books, 2)
}

func TestDenormalizeMissing(t *testing.T) {
	// Denormalizing relationships on objects that are missing should no-op.
	ctx := context.Background()

	authorID := int64(1)
	workID := int64(2)
	bookID := int64(3)

	cache := newMemoryCache()

	notFoundGetter := NewMockgetter(gomock.NewController(t))
	notFoundGetter.EXPECT().GetAuthor(gomock.Any(), authorID).Return(nil, errNotFound).AnyTimes()
	notFoundGetter.EXPECT().GetWork(gomock.Any(), workID, nil).Return(nil, 0, errNotFound).AnyTimes()

	ctrl, err := NewController(cache, notFoundGetter, nil)
	require.NoError(t, err)

	err = ctrl.denormalizeEditions(ctx, workID, bookID)
	assert.ErrorIs(t, err, errNotFound)

	err = ctrl.denormalizeWorks(ctx, authorID, workID)
	assert.ErrorIs(t, err, errNotFound)
}

func TestSubtitles(t *testing.T) {
	// Subtitles (i.e. FullTitle) are used in situations where multiple works
	// share the same primary title, or when the work belongs to a series..

	t.Parallel()

	ctx := context.Background()
	c := gomock.NewController(t)
	getter := NewMockgetter(c)

	workDupe1 := workResource{
		ForeignID: 1,
		Title:     "FOO",
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

	workDupe3 := workResource{
		ForeignID:  3,
		Title:      "Foo",
		FullTitle:  "Foo: Third Work",
		ShortTitle: "Foo",
		Books: []bookResource{
			{ForeignID: 30, Title: "Foo", FullTitle: "Foo: Third Edition"},
			{ForeignID: 40, Title: "Foo", FullTitle: ""},
		},
	}

	workDupe4 := workResource{
		ForeignID:  4,
		Title:      "Foo",
		FullTitle:  "Foo: Fourth Work",
		ShortTitle: "Foo",
		Books: []bookResource{
			{ForeignID: 50, Title: "Foo", FullTitle: "Foo: Fourth Edition"},
			{ForeignID: 60, Title: "Foo", FullTitle: ""},
		},
	}

	workUnique := workResource{
		ForeignID: 5,
		Title:     "Bar",
		FullTitle: "Bar: Not Foo",
		Books: []bookResource{
			{ForeignID: 70, Title: "Bar", FullTitle: "Bar: Not Foo"},
			{ForeignID: 80, Title: "Bar", FullTitle: ""},
		},
	}

	workSeries := workResource{
		ForeignID:  6,
		Title:      "Baz",
		FullTitle:  "Baz: The Baz Series #3",
		ShortTitle: "Baz",
		Books: []bookResource{
			{
				ForeignID:  90,
				Title:      "Baz",
				FullTitle:  "Baz: The Baz Series #3",
				ShortTitle: "Baz",
			},
		},
		Series: []SeriesResource{{ForeignID: 1234}},
	}

	author := AuthorResource{ForeignID: 1000, Works: []workResource{
		workDupe1,
		workDupe2,
		workUnique,
		workSeries,
	}}

	workDupe1.Authors = []AuthorResource{author}
	workDupe2.Authors = []AuthorResource{author}
	workDupe3.Authors = []AuthorResource{author}
	workDupe4.Authors = []AuthorResource{author}
	workUnique.Authors = []AuthorResource{author}
	workSeries.Authors = []AuthorResource{author}

	initialAuthorBytes, err := json.Marshal(author)
	require.NoError(t, err)
	initialWorkDupe1Bytes, err := json.Marshal(workDupe1)
	require.NoError(t, err)
	initialWorkDupe2Bytes, err := json.Marshal(workDupe2)
	require.NoError(t, err)
	initialWorkDupe3Bytes, err := json.Marshal(workDupe3)
	require.NoError(t, err)
	initialWorkDupe4Bytes, err := json.Marshal(workDupe4)
	require.NoError(t, err)
	initialWorkUniqueBytes, err := json.Marshal(workUnique)
	require.NoError(t, err)
	initialWorkSeriesBytes, err := json.Marshal(workSeries)
	require.NoError(t, err)

	cache := newMemoryCache()

	ctrl, err := NewController(cache, getter, nil)
	require.NoError(t, err)

	getter.EXPECT().GetAuthor(gomock.Any(), author.ForeignID).DoAndReturn(func(ctx context.Context, authorID int64) ([]byte, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, AuthorKey(authorID))
		if ok {
			return cachedBytes, nil
		}
		return initialAuthorBytes, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workDupe1.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkDupe1Bytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workDupe2.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkDupe2Bytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workDupe3.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkDupe3Bytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workDupe4.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkDupe4Bytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workUnique.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkUniqueBytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workSeries.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkSeriesBytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetAuthorBooks(gomock.Any(), author.ForeignID).Return(iter.Seq[int64](func(func(int64) bool) {}))

	err = ctrl.denormalizeWorks(ctx, author.ForeignID, workDupe1.ForeignID, workDupe2.ForeignID, workUnique.ForeignID)
	require.NoError(t, err)

	// Add these after the others have already had subtitles applied. We should
	// still apply a subtitle to this new work, instead of using its short
	// title.
	err = ctrl.denormalizeWorks(ctx, author.ForeignID, workDupe3.ForeignID)
	require.NoError(t, err)
	err = ctrl.denormalizeWorks(ctx, author.ForeignID, workDupe4.ForeignID)
	require.NoError(t, err)

	authorBytes, _, err := ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)

	require.NoError(t, json.Unmarshal(authorBytes, &author))

	assert.Equal(t, "Foo: First Work", author.Works[0].Title)
	assert.Equal(t, "Foo: Second Work", author.Works[1].Title)
	assert.Equal(t, "Foo: Third Work", author.Works[2].Title)
	assert.Equal(t, "Foo: Fourth Work", author.Works[3].Title)
	assert.Equal(t, "Bar", author.Works[4].Title)

	assert.Equal(t, "Foo: First Edition", author.Works[0].Books[0].Title)
	assert.Equal(t, "Foo", author.Works[0].Books[1].Title)

	assert.Equal(t, "Foo: Second Edition", author.Works[1].Books[0].Title)
	assert.Equal(t, "Foo", author.Works[1].Books[1].Title)

	assert.Equal(t, "Foo: Third Edition", author.Works[2].Books[0].Title)
	assert.Equal(t, "Foo", author.Works[2].Books[1].Title)

	assert.Equal(t, "Foo: Fourth Edition", author.Works[3].Books[0].Title)
	assert.Equal(t, "Foo", author.Works[3].Books[1].Title)

	assert.Equal(t, "Bar", author.Works[4].Books[0].Title)
	assert.Equal(t, "Bar", author.Works[4].Books[1].Title)

	assert.Equal(t, "Baz: The Baz Series #3", author.Works[5].Books[0].Title)
}

func TestMergedEditions(t *testing.T) {
	// GetBook(X) and GetBook(Y) can both return an edition with ID X if the
	// editions were merged. That shouldn't manifest as a work containing two
	// copies of the same edition, because the client requires uniqueness.
	ctx := t.Context()
	c := gomock.NewController(t)
	getter := NewMockgetter(c)
	cache := newMemoryCache()
	ctrl, err := NewController(cache, getter, nil)
	require.NoError(t, err)

	bookID := int64(1)
	mergedID := int64(2)
	workID := int64(10)
	authorID := int64(100)

	bookBytes, err := json.Marshal(workResource{
		ForeignID: workID,
		Books: []bookResource{{
			ForeignID: bookID,
		}},
	})
	require.NoError(t, err)

	// Treat editions 1 and 2 as merged.
	getter.EXPECT().GetBook(gomock.Any(), bookID, nil).Return(bookBytes, workID, authorID, nil)
	getter.EXPECT().GetBook(gomock.Any(), mergedID, nil).Return(bookBytes, workID, authorID, nil)

	// Treat 1 as the work's best book.
	getter.EXPECT().GetWork(gomock.Any(), workID, nil).Return(bookBytes, authorID, nil)

	err = ctrl.denormalizeEditions(ctx, workID, bookID, mergedID)
	require.NoError(t, err)

	// The work shouldn't have a duplicated edition.
	workBytes, _, err := ctrl.GetWork(ctx, workID)
	require.NoError(t, err)

	var work workResource
	require.NoError(t, json.Unmarshal(workBytes, &work))

	assert.Len(t, work.Books, 1)
}

func TestMergedWorks(t *testing.T) {
	// Same principle as TestMergedEditions.

	ctx := t.Context()
	c := gomock.NewController(t)
	getter := NewMockgetter(c)
	cache := newMemoryCache()
	ctrl, err := NewController(cache, getter, nil)
	require.NoError(t, err)

	workID := int64(1)
	mergedID := int64(2)
	authorID := int64(100)

	workBytes, err := json.Marshal(workResource{
		ForeignID: workID,
		Books:     []bookResource{{ForeignID: 1000}},
	})
	require.NoError(t, err)

	authorBytes, err := json.Marshal(AuthorResource{
		ForeignID: authorID,
	})
	require.NoError(t, err)

	// Treat works 1 and 2 as merged.
	getter.EXPECT().GetWork(gomock.Any(), workID, nil).Return(workBytes, authorID, nil)
	getter.EXPECT().GetWork(gomock.Any(), mergedID, nil).Return(workBytes, authorID, nil)

	getter.EXPECT().GetAuthor(gomock.Any(), authorID).Return(authorBytes, nil)
	getter.EXPECT().GetAuthorBooks(gomock.Any(), authorID).Return(nil)

	err = ctrl.denormalizeWorks(ctx, authorID, workID, mergedID)
	require.NoError(t, err)

	// The author shouldn't have a duplicated work.
	authorBytes, _, err = ctrl.GetAuthor(ctx, authorID)
	require.NoError(t, err)

	var author AuthorResource
	require.NoError(t, json.Unmarshal(authorBytes, &author))

	assert.Len(t, author.Works, 1)
}

func TestFuzz(t *testing.T) {
	fuzzed := fuzz(_authorTTL, 2)
	assert.Less(t, fuzzed, _authorTTL*2)
	assert.Greater(t, fuzzed, _authorTTL)
}

func waitForDenorm(ctrl *Controller) {
	for ctrl.refreshWaiting.Load() != 0 {
		time.Sleep(100 * time.Millisecond)
	}
	for ctrl.grouper.denormWaiting.Load() != 0 {
		time.Sleep(100 * time.Millisecond)
	}

	if os.Getenv("CI") != "" {
		time.Sleep(1 * time.Second)
	} else {
		time.Sleep(100 * time.Millisecond)
	}
}
