//go:generate go run go.uber.org/mock/mockgen -typed -source hardcover_test.go -package hardcover -destination hardcover/mock.go . gql
package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/blampe/rreading-glasses/hardcover"
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

	gql := hardcover.NewMockgql(c)
	gql.EXPECT().MakeRequest(gomock.Any(),
		gomock.AssignableToTypeOf(&graphql.Request{}),
		gomock.AssignableToTypeOf(&graphql.Response{})).DoAndReturn(
		func(ctx context.Context, req *graphql.Request, res *graphql.Response) error {
			if req.OpName == "GetWork" {
				gwr, ok := res.Data.(*hardcover.GetWorkResponse)
				if !ok {
					panic(gwr)
				}
				gwr.Books_by_pk.Editions = []hardcover.GetWorkBooks_by_pkBooksEditions{{
					EditionInfo: hardcover.EditionInfo{
						Id:             30405274,
						Title:          "Out of My Mind",
						Asin:           "",
						Isbn_13:        "9781416971702",
						Edition_format: "Hardcover",
						Pages:          295,
						Audio_seconds:  0,
						Language: hardcover.EditionInfoLanguageLanguages{
							Code3: "eng",
						},
						Publisher: hardcover.EditionInfoPublisherPublishers{
							Name: "Atheneum",
						},
						Release_date: "2010-01-01",
						Book_id:      141397,
					},
				}}
				gwr.Books_by_pk.WorkInfo = hardcover.WorkInfo{
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
					DefaultEditions: hardcover.DefaultEditions{
						Contributions: []hardcover.DefaultEditionsContributions{
							{
								Contributions: hardcover.Contributions{
									Author: hardcover.ContributionsAuthorAuthors{
										AuthorInfo: hardcover.AuthorInfo{
											Id:           51942,
											Name:         "Sharon M. Draper",
											Slug:         "sharon-m-draper",
											Cached_image: json.RawMessage("https://assets.hardcover.app/books/97020/10748148-L.jpg"),
										},
									},
								},
							},
						},
						Default_cover_edition: hardcover.DefaultEditionsDefault_cover_editionEditions{
							Id: 30405274,
							Contributions: []hardcover.DefaultEditionsDefault_cover_editionEditionsContributions{
								{
									Contributions: hardcover.Contributions{
										Author: hardcover.ContributionsAuthorAuthors{
											AuthorInfo: hardcover.AuthorInfo{
												Id: 51942,
											},
										},
									},
								},
							},
						},
					},
					Slug: "out-of-my-mind",
					Book_series: []hardcover.WorkInfoBook_series{
						{
							Position: 1,
							Series: hardcover.WorkInfoBook_seriesSeries{
								Id:   141397,
								Name: "Out of My Mind",
							},
						},
					},
					Rating:        4.111111111111111,
					Ratings_count: 63,
				}

				return nil

			}

			if req.OpName == "GetEdition" {
				ge, ok := res.Data.(*hardcover.GetEditionResponse)
				if !ok {
					panic(ge)
				}
				ge.Editions_by_pk = hardcover.GetEditionEditions_by_pkEditions{
					EditionInfo: hardcover.EditionInfo{
						Id: 30405274,
					},
					Book: hardcover.GetEditionEditions_by_pkEditionsBookBooks{
						WorkInfo: hardcover.WorkInfo{
							Id: 141397,
							DefaultEditions: hardcover.DefaultEditions{
								Contributions: []hardcover.DefaultEditionsContributions{
									{
										Contributions: hardcover.Contributions{
											Author: hardcover.ContributionsAuthorAuthors{
												AuthorInfo: hardcover.AuthorInfo{
													Id: 51942,
												},
											},
										},
									},
								},
								Default_cover_edition: hardcover.DefaultEditionsDefault_cover_editionEditions{
									Id: 30405274,
									Contributions: []hardcover.DefaultEditionsDefault_cover_editionEditionsContributions{
										{
											Contributions: hardcover.Contributions{
												Author: hardcover.ContributionsAuthorAuthors{
													AuthorInfo: hardcover.AuthorInfo{
														Id: 51942,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				}

				return nil
			}
			if req.OpName == "GetAuthorEditions" {
				gaw, ok := res.Data.(*hardcover.GetAuthorEditionsResponse)
				if !ok {
					panic(gaw)
				}
				gaw.Authors_by_pk = hardcover.GetAuthorEditionsAuthors_by_pkAuthors{
					AuthorInfo: hardcover.AuthorInfo{
						Id:   51942,
						Slug: "sharon-m-draper",
					},
					Contributions: []hardcover.GetAuthorEditionsAuthors_by_pkAuthorsContributions{
						{
							Contributions: hardcover.Contributions{
								Author: hardcover.ContributionsAuthorAuthors{
									AuthorInfo: hardcover.AuthorInfo{
										Id: 51942,
									},
								},
								Contribution: "",
							},
							Book: hardcover.GetAuthorEditionsAuthors_by_pkAuthorsContributionsBookBooks{
								DefaultEditions: hardcover.DefaultEditions{
									Contributions: []hardcover.DefaultEditionsContributions{
										{
											Contributions: hardcover.Contributions{
												Author: hardcover.ContributionsAuthorAuthors{
													AuthorInfo: hardcover.AuthorInfo{
														Id: 51942,
													},
												},
											},
										},
									},
									Default_cover_edition: hardcover.DefaultEditionsDefault_cover_editionEditions{
										Id: 30405274,
										Contributions: []hardcover.DefaultEditionsDefault_cover_editionEditionsContributions{
											{
												Contributions: hardcover.Contributions{
													Author: hardcover.ContributionsAuthorAuthors{
														AuthorInfo: hardcover.AuthorInfo{
															Id: 51942,
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				}

				return nil
			}

			return fmt.Errorf("unrecognized op %q", req.OpName)
		}).AnyTimes()

	cache := newMemoryCache()
	getter, err := NewHardcoverGetter(cache, gql)
	require.NoError(t, err)

	ctrl, err := NewController(cache, getter, nil)
	require.NoError(t, err)

	go ctrl.Run(context.Background(), time.Millisecond) // Denormalize data in the background.
	t.Cleanup(func() { ctrl.Shutdown(t.Context()) })

	t.Run("GetBook", func(t *testing.T) {
		bookBytes, ttl, err := ctrl.GetBook(ctx, 30405274)
		require.NoError(t, err)
		assert.NotZero(t, ttl)

		var work workResource
		require.NoError(t, json.Unmarshal(bookBytes, &work))

		assert.Equal(t, int64(141397), work.ForeignID)
		require.Len(t, work.Authors, 1)
		require.Len(t, work.Authors[0].Works, 1)
		assert.Equal(t, int64(51942), work.Authors[0].ForeignID)

		require.Len(t, work.Books, 1)
		assert.Equal(t, int64(30405274), work.Books[0].ForeignID)
	})

	waitForDenorm(ctrl)

	t.Run("GetAuthor", func(t *testing.T) {
		authorBytes, ttl, err := ctrl.GetAuthor(ctx, 51942)
		require.NoError(t, err)
		assert.NotZero(t, ttl)

		// author -> .Works.Authors.Works must not be null, but books can be

		var author AuthorResource
		require.NoError(t, json.Unmarshal(authorBytes, &author))

		assert.Equal(t, int64(51942), author.ForeignID)
		require.Len(t, author.Works, 1)
		require.Len(t, author.Works[0].Authors, 1)
		require.Len(t, author.Works[0].Books, 1)
	})

	t.Run("GetWork", func(t *testing.T) {
		workBytes, ttl, err := ctrl.GetWork(ctx, 141397)
		require.NoError(t, err)
		assert.NotZero(t, ttl)

		var work workResource
		require.NoError(t, json.Unmarshal(workBytes, &work))

		require.Len(t, work.Authors, 1)
		assert.Equal(t, int64(51942), work.Authors[0].ForeignID)
		require.Len(t, work.Authors[0].Works, 1)

		require.Len(t, work.Books, 1)
		assert.Equal(t, int64(30405274), work.Books[0].ForeignID)
	})
}

func TestHardcoverIntegration(t *testing.T) {
	t.Parallel()

	key := os.Getenv("HARDCOVER_API_KEY")
	if key == "" {
		t.Skip("missing HARDCOVER_API_KEY env var")
		return
	}

	cache := newMemoryCache()

	hcTransport := ScopedTransport{
		Host: "api.hardcover.app",
		RoundTripper: &HeaderTransport{
			Key:          "Authorization",
			Value:        "Bearer " + key,
			RoundTripper: http.DefaultTransport,
		},
	}

	hcClient := &http.Client{Transport: hcTransport}

	gql, err := NewBatchedGraphQLClient("https://api.hardcover.app/v1/graphql", hcClient, time.Second, 25)
	require.NoError(t, err)

	getter, err := NewHardcoverGetter(cache, gql)
	require.NoError(t, err)

	ctrl, err := NewController(cache, getter, nil)
	require.NoError(t, err)
	go ctrl.Run(t.Context(), time.Second)

	t.Run("GetAuthor", func(t *testing.T) {
		t.Parallel()
		authorBytes, ttl, err := ctrl.GetAuthor(t.Context(), 91460)
		require.NoError(t, err)
		assert.NotZero(t, ttl)

		var author AuthorResource
		err = json.Unmarshal(authorBytes, &author)
		assert.NoError(t, err)

		assert.Equal(t, int64(91460), author.ForeignID)
		assert.Equal(t, "https://hardcover.app/authors/cormac-mccarthy", author.URL)
		assert.NotEmpty(t, author.Works)
	})

	t.Run("GetBook", func(t *testing.T) {
		t.Parallel()
		bookBytes, ttl, err := ctrl.GetBook(t.Context(), 642392)
		assert.NoError(t, err)
		assert.NotZero(t, ttl)

		var work workResource
		err = json.Unmarshal(bookBytes, &work)
		assert.NoError(t, err)

		assert.Equal(t, int64(642392), work.Books[0].ForeignID)
		assert.Equal(t, int64(36087), work.ForeignID)
		assert.Equal(t, int64(91460), work.Authors[0].ForeignID)
	})

	t.Run("GetWork", func(t *testing.T) {
		t.Parallel()
		workBytes, ttl, err := ctrl.GetWork(t.Context(), 36087)
		assert.NoError(t, err)
		assert.NotZero(t, ttl)

		var work workResource
		err = json.Unmarshal(workBytes, &work)
		assert.NoError(t, err)

		assert.Equal(t, int64(36087), work.ForeignID)
		assert.Equal(t, int64(91460), work.Authors[0].ForeignID)
	})

	t.Run("GetAuthorBooks", func(t *testing.T) {
		t.Parallel()
		iter := getter.GetAuthorBooks(t.Context(), 91460)
		gotBook := false
		for workID := range iter {
			if workID == 30713111 {
				gotBook = true
			}
		}
		assert.True(t, gotBook)
	})

	t.Run("Authorship", func(t *testing.T) {
		// A work with multiple authors should pick a reasonable primary author. Overlord #6 shares authorship with the illustrator.
		workBytes, _, err := ctrl.GetWork(t.Context(), 885684)
		require.NoError(t, err)

		var work workResource
		err = json.Unmarshal(workBytes, &work)
		assert.NoError(t, err)

		assert.Equal(t, int64(259787), work.Authors[0].ForeignID)
	})

	t.Run("Search", func(t *testing.T) {
		t.Parallel()
		results, err := getter.Search(t.Context(), "the crossing")
		require.NoError(t, err)

		expected := SearchResource{
			BookID: 30713122,
			WorkID: 369140,
			Author: SearchResourceAuthor{
				ID: 91460,
			},
		}
		assert.Contains(t, results, expected)
	})

	t.Run("Series (unnumbered)", func(t *testing.T) {
		t.Parallel()
		series, err := getter.GetSeries(t.Context(), 8781)
		require.NoError(t, err)

		assert.Greater(t, len(series.LinkItems), 1000)
		assert.Equal(t, "Warhammer 40,000", series.Title)
	})

	t.Run("Series (numbered)", func(t *testing.T) {
		t.Parallel()
		series, err := getter.GetSeries(t.Context(), 40337)
		require.NoError(t, err)

		assert.Equal(t, len(series.LinkItems), 15)
	})
}
