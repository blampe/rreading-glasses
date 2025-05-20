package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/blampe/rreading-glasses/gr"
	"github.com/blampe/rreading-glasses/hardcover"
	"github.com/eko/gocache/lib/v4/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"go.uber.org/mock/gomock"
)

func TestGetAuthorIntegrity(t *testing.T) {
	// Try to repro null "books" on author.Works
	// 1. load book first, then author?
	// 2. load author first?
}

func TestGRGetBookDataIntegrity(t *testing.T) {
	// The client is particularly sensitive to null values.
	// For a given work resource, it MUST
	// - have non-null top-level books
	// - non-null ratingcount, averagerating
	// - have a contributor with a foreign id

	t.Parallel()

	ctx := context.Background()
	c := gomock.NewController(t)

	upstream := hardcover.NewMocktransport(c)
	upstream.EXPECT().RoundTrip(gomock.Any()).DoAndReturn(func(r *http.Request) (*http.Response, error) {
		if r.Method == "HEAD" {
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
			}
			resp.Header.Add("location", "https://www.gr.com/book/show/6609765-out-of-my-mind")
			return resp, nil
		}
		if r.Method == "GET" {
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`<a class="bookTitle" href="6609765"></a>`)),
			}
			return resp, nil
		}
		panic(r)
	}).AnyTimes()

	gql := hardcover.NewMockgql(c)
	gql.EXPECT().MakeRequest(gomock.Any(),
		gomock.AssignableToTypeOf(&graphql.Request{}),
		gomock.AssignableToTypeOf(&graphql.Response{})).DoAndReturn(
		func(ctx context.Context, req *graphql.Request, res *graphql.Response) error {
			if req.OpName == "GetBook" {
				gbr, ok := res.Data.(*gr.GetBookResponse)
				if !ok {
					panic(gbr)
				}
				gbr.GetBookByLegacyId = gr.GetBookGetBookByLegacyIdBook{
					Id:          "kca://book/amzn1.gr.book.v1.WY3sni8ilbLc2WGHV0N3SQ",
					LegacyId:    6609765,
					Description: "Melody is not like most people. She cannot walk or talk, but she has a photographic memory; she can remember every detail of everything she has ever experienced. She is smarter than most of the adults who try to diagnose her and smarter than her classmates in her integrated classroom - the very same classmates who dismiss her as mentally challenged because she cannot tell them otherwise. But Melody refuses to be defined by cerebral palsy. And she's determined to let everyone know it - somehow.",
					BookGenres: []gr.GetBookGetBookByLegacyIdBookBookGenresBookGenre{
						{Genre: gr.GetBookGetBookByLegacyIdBookBookGenresBookGenreGenre{Name: "Young Adult"}},
					},
					BookSeries: []gr.GetBookGetBookByLegacyIdBookBookSeries{
						{
							SeriesPlacement: "1",
							Series: gr.GetBookGetBookByLegacyIdBookBookSeriesSeries{
								Id:     "kca://series/amzn1.gr.series.v3.owomqLJFO4sueLJt",
								Title:  "Out of My Mind",
								WebUrl: "https://www.gr.com/series/326523-out-of-my-mind",
							},
						},
					},
					Details: gr.GetBookGetBookByLegacyIdBookDetails{
						Asin:     "141697170X",
						Isbn13:   "9781416971702",
						Format:   "Hardcover",
						NumPages: 295,
						Language: gr.GetBookGetBookByLegacyIdBookDetailsLanguage{
							Name: "English",
						},
						OfficialUrl:     "",
						Publisher:       "Atheneum Books for Young Readers",
						PublicationTime: 1268121600000,
					},
					ImageUrl: "https://images-na.ssl-images-amazon.com/images/S/compressed.photo.gr.com/books/1347602096i/6609765.jpg",
					PrimaryContributorEdge: gr.GetBookGetBookByLegacyIdBookPrimaryContributorEdgeBookContributorEdge{
						Node: gr.GetBookGetBookByLegacyIdBookPrimaryContributorEdgeBookContributorEdgeNodeContributor{
							Id:   "kca://author/amzn1.gr.author.v1.tnLKwFVJefdFsJ6d34fT6Q",
							Name: "Sharon M. Draper",

							LegacyId:        51942,
							WebUrl:          "https://www.gr.com/author/show/51942.Sharon_M_Draper",
							ProfileImageUrl: "https://i.gr-assets.com/images/S/compressed.photo.gr.com/authors/1236906847i/51942._UX200_CR0,49,200,200_.jpg",
							Description:     "<i>Sharon M. Draper</i> is a professional educator as well as an accomplished writer. She has been honored as the National Teacher of the Year, is a five-time winner of the Coretta Scott King Literary Award, and is a New York Times bestselling author. She lives in Cincinnati, Ohio.",
						},
					},
					Stats: gr.GetBookGetBookByLegacyIdBookStatsBookOrWorkStats{
						AverageRating: 4.35,
						RatingsCount:  156543,
						RatingsSum:    680605,
					},
					TitlePrimary: "Out of My Mind",
					WebUrl:       "https://www.gr.com/book/show/6609765-out-of-my-mind",
					Work: gr.GetBookGetBookByLegacyIdBookWork{
						Id:       "kca://work/amzn1.gr.work.v1.DaUnQI3cWL066Bo8_EL8-A",
						LegacyId: 6803732,
						Details: gr.GetBookGetBookByLegacyIdBookWorkDetails{
							WebUrl:          "https://www.gr.com/work/6803732-out-of-my-mind",
							PublicationTime: 1268121600000,
						},
						BestBook: gr.GetBookGetBookByLegacyIdBookWorkBestBook{
							LegacyId: 6609765,
						},
					},
				}
				return nil

			}
			if req.OpName == "GetAuthorWorks" {
				gaw, ok := res.Data.(*gr.GetAuthorWorksResponse)
				if !ok {
					panic(gaw)
				}
				gaw.GetWorksByContributor = gr.GetAuthorWorksGetWorksByContributorContributorWorksConnection{
					Edges: []gr.GetAuthorWorksGetWorksByContributorContributorWorksConnectionEdgesContributorWorksEdge{{
						Node: gr.GetAuthorWorksGetWorksByContributorContributorWorksConnectionEdgesContributorWorksEdgeNodeWork{
							Id: "kca://work/amzn1.gr.work.v1.DaUnQI3cWL066Bo8_EL8-A",
							BestBook: gr.GetAuthorWorksGetWorksByContributorContributorWorksConnectionEdgesContributorWorksEdgeNodeWorkBestBook{
								LegacyId: 6609765,
								PrimaryContributorEdge: gr.GetAuthorWorksGetWorksByContributorContributorWorksConnectionEdgesContributorWorksEdgeNodeWorkBestBookPrimaryContributorEdgeBookContributorEdge{
									Role: "Author",
									Node: gr.GetAuthorWorksGetWorksByContributorContributorWorksConnectionEdgesContributorWorksEdgeNodeWorkBestBookPrimaryContributorEdgeBookContributorEdgeNodeContributor{
										LegacyId: 51942,
									},
								},
								SecondaryContributorEdges: []gr.GetAuthorWorksGetWorksByContributorContributorWorksConnectionEdgesContributorWorksEdgeNodeWorkBestBookSecondaryContributorEdgesBookContributorEdge{},
							},
						},
					}},
				}
			}
			return nil
		}).AnyTimes()

	cache := &LayeredCache{wrapped: []cache.SetterCacheInterface[[]byte]{newMemory()}}
	getter, err := NewGRGetter(cache, gql, &http.Client{Transport: upstream})
	require.NoError(t, err)

	ctrl, err := NewController(cache, getter)
	require.NoError(t, err)

	// go ctrl.Run(context.Background())

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

	t.Run("GetAuthor", func(t *testing.T) {
		authorBytes, err := ctrl.GetAuthor(ctx, 51942)
		assert.NoError(t, err)

		// author -> .Works.Authors.Works must not be null, but books can be

		var author AuthorResource
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

func TestReleaseDate(t *testing.T) {
	tests := []struct {
		given float64
		want  string
	}{
		{
			given: 715935600000,
			want:  "1992-09-08 07:00:00",
		},
		{
			// C#'s DateTime.Parse doesn't handle years before 1AD, so we omit
			// them.
			given: -73212048000000,
			want:  "",
		},
		{
			given: -62135596700000,
			want:  "0001-01-01 00:01:40",
		},
		{
			given: -62135596800000,
			want:  "0001-01-01 00:00:00",
		},
		{
			given: -62135596900000,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.given), func(t *testing.T) {
			got := releaseDate(tt.given)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBatchError(t *testing.T) {
	// If one of our results returns a 404, the other results should still succeed.

	host := os.Getenv("GRHOST")
	if host == "" {
		t.Skip("missing GRHOST env var")
		return
	}

	upstream, err := NewUpstream(host, "", "")
	require.NoError(t, err)

	gql, err := NewGRGQL(t.Context(), upstream, "")
	require.NoError(t, err)

	var err1, err2 error

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err1 = gr.GetAuthorWorks(t.Context(), gql, gr.GetWorksByContributorInput{
			Id: "kca://author/amzn1.gr.author.v1.lDq44Mxx0gBfWyqfZwEI1Q",
		}, gr.PaginationInput{Limit: 1})
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err2 = gr.GetAuthorWorks(t.Context(), gql, gr.GetWorksByContributorInput{
			Id: "kca://author",
		}, gr.PaginationInput{Limit: 1})
	}()

	wg.Wait()

	assert.NoError(t, err1)

	gqlErr := &gqlerror.Error{}
	assert.ErrorAs(t, err2, &gqlErr)
}

func TestAuth(t *testing.T) {
	t.Parallel()

	// Sanity check that we're authorized for all relevant endpoints.
	host := os.Getenv("GRHOST")
	if host == "" {
		t.Skip("missing GRHOST env var")
		return
	}

	cookie := os.Getenv("GR_TEST_COOKIE")
	if cookie == "" {
		t.Skip("missing GR_TEST_COOKIE")
		return
	}

	cache := &LayeredCache{wrapped: []cache.SetterCacheInterface[[]byte]{newMemory()}}

	upstream, err := NewUpstream(host, cookie, "")
	require.NoError(t, err)

	gql, err := NewGRGQL(t.Context(), upstream, cookie)
	require.NoError(t, err)

	getter, err := NewGRGetter(cache, gql, upstream)
	require.NoError(t, err)
	ctrl, err := NewController(cache, getter)
	go ctrl.Run(t.Context(), time.Second)

	require.NoError(t, err)

	t.Run("GetAuthor", func(t *testing.T) {
		t.Parallel()
		_, err := ctrl.GetAuthor(t.Context(), 4178)
		assert.NoError(t, err)
	})

	t.Run("GetBook", func(t *testing.T) {
		t.Parallel()
		_, err := ctrl.GetBook(t.Context(), 394535)
		assert.NoError(t, err)
	})

	t.Run("GetWork", func(t *testing.T) {
		t.Parallel()
		_, err := ctrl.GetWork(t.Context(), 1930437)
		assert.NoError(t, err)
	})

	t.Run("GetAuthorBooks", func(t *testing.T) {
		t.Parallel()
		iter := getter.GetAuthorBooks(t.Context(), 4178)
		gotBook := false
		for range iter {
			gotBook = true
			break
		}
		assert.True(t, gotBook)
	})
}
