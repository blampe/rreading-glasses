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

	dupeEditionID := int64(123)

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
				// We shouldn't re-query for any other books except the one we
				// originally fetched.
				if id := req.Variables.(interface{ GetLegacyId() int64 }).GetLegacyId(); id != 6609765 {
					panic(id)
				}
				gbr, ok := res.Data.(*gr.GetBookResponse)
				if !ok {
					panic(gbr)
				}
				gbr.GetBookByLegacyId = gr.GetBookGetBookByLegacyIdBook{
					BookInfo: gr.BookInfo{
						Id:          "kca://book/amzn1.gr.book.v1.WY3sni8ilbLc2WGHV0N3SQ",
						LegacyId:    6609765,
						Description: "Melody is not like most people. She cannot walk or talk, but she has a photographic memory; she can remember every detail of everything she has ever experienced. She is smarter than most of the adults who try to diagnose her and smarter than her classmates in her integrated classroom - the very same classmates who dismiss her as mentally challenged because she cannot tell them otherwise. But Melody refuses to be defined by cerebral palsy. And she's determined to let everyone know it - somehow.",
						BookGenres: []gr.BookInfoBookGenresBookGenre{
							{Genre: gr.BookInfoBookGenresBookGenreGenre{Name: "Young Adult"}},
						},
						BookSeries: []gr.BookInfoBookSeries{
							{
								SeriesPlacement: "1",
								Series: gr.BookInfoBookSeriesSeries{
									Id:     "kca://series/amzn1.gr.series.v3.owomqLJFO4sueLJt",
									Title:  "Out of My Mind",
									WebUrl: "https://www.gr.com/series/326523-out-of-my-mind",
								},
							},
						},
						Details: gr.BookInfoDetailsBookDetails{
							Asin:     "141697170X",
							Isbn13:   "9781416971702",
							Format:   "Hardcover",
							NumPages: 295,
							Language: gr.BookInfoDetailsBookDetailsLanguage{
								Name: "English",
							},
							OfficialUrl:     "",
							Publisher:       "Atheneum Books for Young Readers",
							PublicationTime: 1268121600000,
						},
						ImageUrl: "https://images-na.ssl-images-amazon.com/images/S/compressed.photo.gr.com/books/1347602096i/6609765.jpg",
						PrimaryContributorEdge: gr.BookInfoPrimaryContributorEdgeBookContributorEdge{
							Node: gr.BookInfoPrimaryContributorEdgeBookContributorEdgeNodeContributor{
								Id:   "kca://author/amzn1.gr.author.v1.tnLKwFVJefdFsJ6d34fT6Q",
								Name: "Sharon M. Draper",

								LegacyId:        51942,
								WebUrl:          "https://www.gr.com/author/show/51942.Sharon_M_Draper",
								ProfileImageUrl: "https://i.gr-assets.com/images/S/compressed.photo.gr.com/authors/1236906847i/51942._UX200_CR0,49,200,200_.jpg",
								Description:     "<i>Sharon M. Draper</i> is a professional educator as well as an accomplished writer. She has been honored as the National Teacher of the Year, is a five-time winner of the Coretta Scott King Literary Award, and is a New York Times bestselling author. She lives in Cincinnati, Ohio.",
							},
						},
						Stats: gr.BookInfoStatsBookOrWorkStats{
							AverageRating: 4.35,
							RatingsCount:  156543,
							RatingsSum:    680605,
						},
						TitlePrimary: "Out of My Mind",
						WebUrl:       "https://www.gr.com/book/show/6609765-out-of-my-mind",
					},
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
						Editions: gr.GetBookGetBookByLegacyIdBookWorkEditionsBooksConnection{
							Edges: []gr.GetBookGetBookByLegacyIdBookWorkEditionsBooksConnectionEdgesBooksEdge{
								{
									Node: gr.GetBookGetBookByLegacyIdBookWorkEditionsBooksConnectionEdgesBooksEdgeNodeBook{
										BookInfo: gr.BookInfo{
											LegacyId: 6609765,
											Title:    "Out of My Mind",
											Details: gr.BookInfoDetailsBookDetails{
												Language: gr.BookInfoDetailsBookDetailsLanguage{
													Name: "English",
												},
											},
										},
									},
								},
								{
									Node: gr.GetBookGetBookByLegacyIdBookWorkEditionsBooksConnectionEdgesBooksEdgeNodeBook{
										BookInfo: gr.BookInfo{
											LegacyId: dupeEditionID, // Should be ignored since this is a dupe.
											Title:    "OUT OF MY MIND",
											Details: gr.BookInfoDetailsBookDetails{
												Language: gr.BookInfoDetailsBookDetailsLanguage{
													Name: "English",
												},
											},
										},
									},
								},
								{
									Node: gr.GetBookGetBookByLegacyIdBookWorkEditionsBooksConnectionEdgesBooksEdgeNodeBook{
										BookInfo: gr.BookInfo{
											LegacyId: 6609766, // Should be included since it's a different language.
											Title:    "Some other edition",
											Details: gr.BookInfoDetailsBookDetails{
												Language: gr.BookInfoDetailsBookDetailsLanguage{
													Name: "German",
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

	cache := newMemoryCache()
	getter, err := NewGRGetter(cache, gql, &http.Client{Transport: upstream})
	require.NoError(t, err)

	ctrl, err := NewController(cache, getter)
	require.NoError(t, err)

	go ctrl.Run(t.Context(), 0)
	t.Cleanup(func() { ctrl.Shutdown(t.Context()) })

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

		assert.Equal(t, "eng", work.Books[0].Language)
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
		// Make sure our cache is empty so we actually exercise the work refresh.
		require.NoError(t, ctrl.cache.Expire(t.Context(), WorkKey(6803732)))
		require.NoError(t, ctrl.cache.Expire(t.Context(), BookKey(6609765)))

		_, err := ctrl.GetWork(ctx, 6803732)
		assert.NoError(t, err)

		time.Sleep(10 * time.Millisecond)

		workBytes, err := ctrl.GetWork(ctx, 6803732)
		assert.NoError(t, err)

		var work workResource
		require.NoError(t, json.Unmarshal(workBytes, &work))

		require.Len(t, work.Authors, 1)
		assert.Equal(t, int64(51942), work.Authors[0].ForeignID)
		require.Len(t, work.Authors[0].Works, 1)

		require.Len(t, work.Books, 2)
		assert.Equal(t, int64(6609765), work.Books[0].ForeignID)
		assert.Equal(t, int64(6609766), work.Books[1].ForeignID)
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
			// Similarly dates after 9999 are likely typos and should be ignored.
			given: 253402329599999,
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

	host := os.Getenv("GR_HOST")
	if host == "" {
		t.Skip("missing GR_HOST env var")
		return
	}

	upstream, err := NewUpstream(host, "", "")
	require.NoError(t, err)

	gql, err := NewGRGQL(t.Context(), upstream, "", time.Second, 2)
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
	host := os.Getenv("GR_HOST")
	if host == "" {
		t.Skip("missing GR_HOST env var")
		return
	}

	cookie := os.Getenv("GR_TEST_COOKIE")
	if cookie == "" {
		t.Skip("missing GR_TEST_COOKIE")
		return
	}

	cache := newMemoryCache()

	upstream, err := NewUpstream(host, cookie, "")
	require.NoError(t, err)

	gql, err := NewGRGQL(t.Context(), upstream, cookie, time.Second, 6)
	require.NoError(t, err)

	getter, err := NewGRGetter(cache, gql, upstream)
	require.NoError(t, err)
	ctrl, err := NewController(cache, getter)
	go ctrl.Run(t.Context(), time.Second)

	require.NoError(t, err)

	t.Run("GetAuthor", func(t *testing.T) {
		t.Parallel()
		authorBytes, err := ctrl.GetAuthor(t.Context(), 4178)
		assert.NoError(t, err)

		var author AuthorResource
		err = json.Unmarshal(authorBytes, &author)
		assert.NoError(t, err)

		assert.Equal(t, int64(4178), author.ForeignID)
	})

	t.Run("GetBook", func(t *testing.T) {
		t.Parallel()
		bookBytes, err := ctrl.GetBook(t.Context(), 394535)
		assert.NoError(t, err)

		var work workResource
		err = json.Unmarshal(bookBytes, &work)
		assert.NoError(t, err)

		assert.Equal(t, int64(394535), work.Books[0].ForeignID)
	})

	t.Run("GetWork", func(t *testing.T) {
		t.Parallel()
		workBytes, err := ctrl.GetWork(t.Context(), 1930437)
		assert.NoError(t, err)

		var work workResource
		err = json.Unmarshal(workBytes, &work)
		assert.NoError(t, err)

		assert.Equal(t, int64(1930437), work.ForeignID)
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
