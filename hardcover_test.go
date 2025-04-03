//go:generate go run go.uber.org/mock/mockgen -typed -source hardcover_test.go -package hardcover -destination hardcover/mock.go . gql
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/blampe/rreading-glasses/hardcover"
	"github.com/eko/gocache/lib/v4/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

//nolint:unused
type gql interface {
	graphql.Client
}

//nolint:unused
type transport interface {
	http.RoundTripper
}

func TestGetBookDataIntegrity(t *testing.T) {
	// The client is particularly sensitive to null values.
	// For a given work resource, it MUST
	// - have non-null top-level books
	// - non-null ratingcount, averagerating
	// - have a contributor with a foreign id

	t.Parallel()

	ctx := context.Background()
	c := gomock.NewController(t)
	upstream := hardcover.NewMocktransport(c)

	gql := hardcover.NewMockgql(c)
	gql.EXPECT().MakeRequest(gomock.Any(),
		gomock.AssignableToTypeOf(&graphql.Request{}),
		gomock.AssignableToTypeOf(&graphql.Response{})).DoAndReturn(
		func(ctx context.Context, req *graphql.Request, res *graphql.Response) error {
			if req.OpName == "GetBook" {
				gbr, ok := res.Data.(*hardcover.GetBookResponse)
				if !ok {
					panic(gbr)
				}
				gbr.Book_mappings = []hardcover.GetBookBook_mappings{
					{
						Edition: hardcover.GetBookBook_mappingsEditionEditions{
							Id:             30405274,
							Title:          "Out of My Mind",
							Asin:           "",
							Isbn_13:        "9781416971702",
							Edition_format: "Hardcover",
							Pages:          295,
							Audio_seconds:  0,
							Language: hardcover.GetBookBook_mappingsEditionEditionsLanguageLanguages{
								Language: "English",
							},
							Publisher: hardcover.GetBookBook_mappingsEditionEditionsPublisherPublishers{
								Name: "Atheneum",
							},
							Release_date: "2010-01-01",
							Description:  "foo",
							// dto_external(path:"identifiers") seems to be more complete
							Identifiers: json.RawMessage(`{
								"asin": [],
								"lccn": [
								  "2009018404"
								],
								"oclc": [
								  "401713291"
								],
								"ocaid": [],
								"isbn_10": [
								  "141697170X"
								],
								"isbn_13": [
								  "9781416971702",
								  "9781416980452"
								],
								"goodreads": [],
								"kindle_asin": [],
								"openlibrary": [
								  "OL24378894M"
								],
								"inventaire_id": []
							  }`),
							Book_id: 141397,
						},
						Book: hardcover.GetBookBook_mappingsBookBooks{
							Id:           141397,
							Title:        "Out of My Mind",
							Description:  "foo",
							Release_date: "2010-01-01",
							Cached_tags: json.RawMessage(`[
									{
									  "tag": "Fiction",
									  "tagSlug": "fiction",
									  "category": "Genre",
									  "categorySlug": "genre",
									  "spoilerRatio": 0,
									  "count": 29758
									},
									{
									  "tag": "Young Adult",
									  "tagSlug": "young-adult",
									  "category": "Genre",
									  "categorySlug": "genre",
									  "spoilerRatio": 0,
									  "count": 22645
									},
									{
									  "tag": "Juvenile Fiction",
									  "tagSlug": "juvenile-fiction",
									  "category": "Genre",
									  "categorySlug": "genre",
									  "spoilerRatio": 0,
									  "count": 3661
									},
									{
									  "tag": "Juvenile Nonfiction",
									  "tagSlug": "juvenile-nonfiction-6a8774e3-9173-46e1-87d7-ea5fa5eb20e8",
									  "category": "Genre",
									  "categorySlug": "genre",
									  "spoilerRatio": 0,
									  "count": 1561
									},
									{
									  "tag": "Family",
									  "tagSlug": "family",
									  "category": "Genre",
									  "categorySlug": "genre",
									  "spoilerRatio": 0,
									  "count": 847
									}
								  ]`),
							Cached_image: json.RawMessage("https://assets.hardcover.app/edition/30405274/d41534ce6075b53289d1c4d57a6dac34b974ce91.jpeg"),
							Contributions: []hardcover.GetBookBook_mappingsBookBooksContributions{
								{
									Contributable_type: "Book",
									Author: hardcover.GetBookBook_mappingsBookBooksContributionsAuthorAuthors{
										Id:           97020,
										Name:         "Sharon M. Draper",
										Slug:         "sharon-m-draper",
										Cached_image: json.RawMessage("https://assets.hardcover.app/books/97020/10748148-L.jpg"),
									},
								},
							},
							Slug: "out-of-my-mind",
							Book_series: []hardcover.GetBookBook_mappingsBookBooksBook_series{
								{
									Position: 1,
									Series: hardcover.GetBookBook_mappingsBookBooksBook_seriesSeries{
										Id:   6143,
										Name: "Out of My Mind",
										Identifiers: json.RawMessage(`{
										  "goodreads": [
											"326523"
										  ]
										}`),
									},
								},
							},
							Rating:        4.111111111111111,
							Ratings_count: 63,
							Book_mappings: []hardcover.GetBookBook_mappingsBookBooksBook_mappings{
								{
									Dto_external: json.RawMessage(`{}`),
								},
								{
									Dto_external: json.RawMessage(`{
										"raw_data": {
											"work": {
												"id": 6803732
											},
											"authors": {
												"author": {
													"id": "51942"
												}
											}
										}
									}`),
								},
							},
						},
					},
				}

				return nil

			}
			if req.OpName == "GetAuthorWorks" {
				gaw, ok := res.Data.(*hardcover.GetAuthorEditionsResponse)
				if !ok {
					panic(gaw)
				}
				gaw.Authors = []hardcover.GetAuthorEditionsAuthors{
					{
						Id:   97020,
						Slug: "sharon-m-draper",
						Contributions: []hardcover.GetAuthorEditionsAuthorsContributions{
							{
								Book: hardcover.GetAuthorEditionsAuthorsContributionsBookBooks{
									Id:            141397,
									Title:         "Out of My Mind",
									Ratings_count: 63,
									Book_mappings: []hardcover.GetAuthorEditionsAuthorsContributionsBookBooksBook_mappings{
										{
											Book_id:     141397,
											Edition_id:  30405274,
											External_id: "6609765",
										},
									},
								},
							},
						},
					},
				}
			}
			return nil
		}).AnyTimes()

	cache := &layeredcache{wrapped: []cache.SetterCacheInterface[[]byte]{newMemory()}}
	getter, err := newHardcoverGetter(cache, gql, &http.Client{Transport: upstream})
	require.NoError(t, err)

	ctrl, err := newController(cache, getter)
	require.NoError(t, err)

	go ctrl.Run(context.Background()) // Denormalize data in the background.

	t.Run("GetBook", func(t *testing.T) {
		bookBytes, err := ctrl.GetBook(ctx, 6609765)
		assert.NoError(t, err)

		var work workResource
		require.NoError(t, json.Unmarshal(bookBytes, &work))

		assert.Equal(t, int64(6803732), work.ForeignID)
		require.Len(t, work.Authors, 1)
		require.Len(t, work.Authors[0].Works, 1)
		assert.Equal(t, int64(51942), work.Authors[0].ForeignID)

		require.Len(t, work.Books, 1)
		assert.Equal(t, int64(6609765), work.Books[0].ForeignID)
	})

	time.Sleep(100 * time.Millisecond) // Wait for data denormalization.

	t.Run("GetAuthor", func(t *testing.T) {
		authorBytes, err := ctrl.GetAuthor(ctx, 51942)
		assert.NoError(t, err)

		// author -> .Works.Authors.Works must not be null, but books can be

		var author authorResource
		require.NoError(t, json.Unmarshal(authorBytes, &author))

		assert.Equal(t, int64(51942), author.ForeignID)
		require.Len(t, author.Works, 1)
		require.Len(t, author.Works[0].Authors, 1)
		require.Len(t, author.Works[0].Books, 1)
	})

	t.Run("GetWork", func(t *testing.T) {
		workBytes, err := ctrl.GetWork(ctx, 6803732)
		assert.NoError(t, err)

		var work workResource
		require.NoError(t, json.Unmarshal(workBytes, &work))

		require.Len(t, work.Authors, 1)
		assert.Equal(t, int64(51942), work.Authors[0].ForeignID)
		require.Len(t, work.Authors[0].Works, 1)

		require.Len(t, work.Books, 1)
		assert.Equal(t, int64(6609765), work.Books[0].ForeignID)
	})
}
