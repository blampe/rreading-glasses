package internal

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/blampe/isbn"
	"github.com/blampe/rreading-glasses/gr"
	"github.com/bytedance/sonic"
	"github.com/microcosm-cc/bluemonday"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/html"
)

var _stripTags = bluemonday.StrictPolicy()

// GRGetter fetches information from a GR upstream.
type GRGetter struct {
	cache    cache[[]byte]
	gql      graphql.Client
	upstream *http.Client
}

var _ getter = (*GRGetter)(nil)

// _grkey key has been public for years.
// https://github.com/search?q=whFzJP3Ud0gZsAdyXxSr7T&type=code
var _grkey = "T7rSxXydAsZg0dU3PJzFhw"

// NewGRGetter creates a new Getter backed by G——R——.
func NewGRGetter(cache cache[[]byte], gql graphql.Client, upstream *http.Client) (*GRGetter, error) {
	return &GRGetter{
		cache:    cache,
		gql:      gql,
		upstream: upstream,
	}, nil
}

// NewGRGQL returns a new GraphQL client for use with GR. The provided
// [http.Client] must be non-nil and is used for issuing requests. If a
// non-empty cookie is given the requests are authorized and use are allowed
// more RPS.
func NewGRGQL(_ context.Context, rate time.Duration, batchSize int, reg *prometheus.Registry) (graphql.Client, error) {
	// These credentials are public and easily obtainable. They are obscured here only to hide them from search results.
	defaultToken, err := hex.DecodeString("6461322d787067736479646b627265676a68707236656a7a716468757779")
	if err != nil {
		return nil, err
	}
	host, err := hex.DecodeString("68747470733a2f2f6b7862776d716f76366a676733646161616d62373434796375342e61707073796e632d6170692e75732d656173742d312e616d617a6f6e6177732e636f6d2f6772617068716c")
	if err != nil {
		return nil, err
	}

	auth := &HeaderTransport{
		Key:   "X-Api-Key",
		Value: string(defaultToken),
		RoundTripper: errorProxyTransport{
			RoundTripper: http.DefaultTransport,
		},
	}
	return NewBatchedGraphQLClient(string(host), &http.Client{Transport: auth}, rate, batchSize, reg)
}

// Search hits the auto_complete API that has been used historically, so it
// returns exactly the same results as legacy.
func (g *GRGetter) Search(ctx context.Context, query string) ([]SearchResource, error) {
	isAsin := _asin.Match([]byte(query))
	isbn, _ := isbn.Parse(query)
	if isAsin || isbn != nil {
		// SearchSuggestions doesn't currently handle ASIN or ISBN. Fall back to an auto_complete query.
		return g.autoComplete(ctx, query)
	}

	resp, err := gr.Search(ctx, g.gql, query)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}

	result := []SearchResource{}

	for _, e := range resp.GetSearchSuggestions.Edges {
		edge, ok := e.(*gr.SearchGetSearchSuggestionsSearchResultsConnectionEdgesSearchBookEdge)
		if !ok {
			continue
		}
		result = append(result, SearchResource{
			BookID: edge.Node.LegacyId,
			WorkID: edge.Node.Work.LegacyId,
			Author: SearchResourceAuthor{
				ID: edge.Node.Work.BestBook.PrimaryContributorEdge.Node.LegacyId,
			},
		})
	}
	return result, nil
}

// autoComplete is the legacy GR search API which handles ASIN and ISBN queries.
func (g *GRGetter) autoComplete(ctx context.Context, query string) ([]SearchResource, error) {
	url := fmt.Sprintf("/book/auto_complete?format=json&q=%s", query)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		Log(ctx).Debug("problem creating auto_complete request", "err", err)
		return nil, err
	}

	resp, err := g.upstream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("doing upstream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		Log(ctx).Warn("problem auto completing", "q", query, "err", err)
		return nil, fmt.Errorf("auto completing %q: %w", query, err)
	}

	var r []struct {
		BookID string `json:"bookId"`
		WorkID string `json:"workId"`
		Author struct {
			ID int64 `json:"id"`
		} `json:"author"`
	}

	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	result := []SearchResource{}

	for _, rr := range r {
		bookID, err := pathToID(rr.BookID)
		if err != nil {
			continue
		}
		workID, err := pathToID(rr.WorkID)
		if err != nil {
			continue
		}
		if rr.Author.ID == 0 {
			continue
		}
		result = append(result, SearchResource{
			WorkID: workID,
			BookID: bookID,
			Author: SearchResourceAuthor{
				ID: rr.Author.ID,
			},
		})
	}

	return result, nil
}

// GetWork returns a work with all known editions. Due to the way R—— works, if
// an edition is missing here (like a translated edition) it's not fetchable.
func (g *GRGetter) GetWork(ctx context.Context, workID int64, saveEditions editionsCallback) (_ []byte, authorID int64, _ error) {
	if workID == 146797269 {
		// This work always 500s for some reason. Ignore it.
		return nil, 0, errNotFound
	}
	workBytes, ttl, ok := g.cache.GetWithTTL(ctx, WorkKey(workID))
	if ok && ttl > 0 {
		return workBytes, 0, nil
	}

	Log(ctx).Debug("getting work", "workID", workID)

	if ok {
		var work workResource
		_ = json.Unmarshal(workBytes, &work)

		bookID := work.BestBookID
		if bookID != 0 {
			Log(ctx).Debug("found cached work", "workID", workID)
			out, _, authorID, err := g.GetBook(ctx, bookID, saveEditions)
			return out, authorID, err
		}
	}

	url := fmt.Sprintf("/work/best_book/%d?key=%s", workID, _grkey)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("requesting best book ID: %w", err)
	}
	resp, err := g.upstream.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("problem getting best book ID: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var r struct {
		BestBook struct {
			ID int64 `xml:"id"`
		} `xml:"best_book"`
	}

	err = xml.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return nil, 0, fmt.Errorf("parsing response: %w", err)
	}

	out, _, authorID, err := g.GetBook(ctx, r.BestBook.ID, saveEditions)
	return out, authorID, err
}

// GetBook fetches a book (edition) from GR.
func (g *GRGetter) GetBook(ctx context.Context, bookID int64, saveEditions editionsCallback) (_ []byte, workID, authorID int64, _ error) {
	if workBytes, ttl, ok := g.cache.GetWithTTL(ctx, BookKey(bookID)); ok && ttl > 0 {
		return workBytes, 0, 0, nil
	}

	Log(ctx).Debug("getting book", "bookID", bookID)

	resp, err := gr.GetBook(ctx, g.gql, bookID)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("getting book: %w", err)
	}

	book := resp.GetBookByLegacyId.BookInfo
	work := resp.GetBookByLegacyId.Work

	workRsc := mapToWorkResource(book, work)

	out, err := json.Marshal(workRsc)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("marshaling work: %w", err)
	}

	// If a work isn't already cached with this ID, and this book is the "best"
	// edition, then write a cache entry using our edition as a starting point.
	// The controller will handle denormalizing this to the author.
	if _, ok := g.cache.Get(ctx, WorkKey(workRsc.ForeignID)); !ok && workRsc.BestBookID == bookID {
		g.cache.Set(ctx, WorkKey(workRsc.ForeignID), out, _workTTL)
	}

	// If this is the "best" edition for the work, then also persist the other
	// (de-duped) editions we have for it.
	if saveEditions != nil && workRsc.BestBookID == bookID {
		editions := map[editionDedupe]workResource{}
		for _, e := range work.Editions.Edges {
			key := editionDedupe{
				title:    strings.ToUpper(e.Node.Title),
				language: iso639_3(e.Node.Details.Language.Name),
				audio:    e.Node.Details.Format == "Audible Audio",
			}
			edition := e.Node.BookInfo
			if _, ok := editions[key]; ok {
				continue // Already saw an edition similar to this one.
			}
			editions[key] = mapToWorkResource(edition, work) // Don't add any more editions like this one.
		}
		saveEditions(slices.Collect(maps.Values(editions))...)
	}

	return out, workRsc.ForeignID, workRsc.Authors[0].ForeignID, nil
}

// mapToWorkResource maps a GR book (edition) to the WorkResource model expected by R.
func mapToWorkResource(book gr.BookInfo, work gr.GetBookGetBookByLegacyIdBookWork) workResource {
	genres := []string{}
	for _, g := range book.BookGenres {
		genres = append(genres, g.Genre.Name)
	}
	if len(genres) == 0 {
		genres = []string{"none"}
	}

	series := []SeriesResource{}
	for _, s := range book.BookSeries {
		legacyID, _ := pathToID(s.Series.WebUrl)
		position, _ := pathToID(s.SeriesPlacement)
		series = append(series, SeriesResource{
			KCA:         s.Series.Id,
			Title:       s.Series.Title,
			ForeignID:   legacyID,
			Description: "TODO", // Would need to scrape this.

			LinkItems: []seriesWorkLinkResource{{
				PositionInSeries: s.SeriesPlacement,
				SeriesPosition:   int(position), // TODO: What's the difference b/t placement?
				ForeignWorkID:    work.LegacyId,
				Primary:          false, // TODO: How can we get this???
			}},
		})
	}

	bookDescription := strings.TrimSpace(book.Description)
	if bookDescription == "" {
		bookDescription = "N/A" // Must be set?
	}

	bookRsc := bookResource{
		KCA:                book.Id,
		ForeignID:          book.LegacyId,
		Asin:               book.Details.Asin,
		Description:        bookDescription,
		Isbn13:             book.Details.Isbn13,
		Title:              book.TitlePrimary,
		FullTitle:          book.Title,
		ShortTitle:         book.TitlePrimary,
		Language:           iso639_3(book.Details.Language.Name),
		Format:             book.Details.Format,
		EditionInformation: "",                     // TODO: Is this used anywhere?
		Publisher:          book.Details.Publisher, // TODO: Ignore books without publishers?
		ImageURL:           book.ImageUrl,
		IsEbook:            book.Details.Format == "Kindle Edition", // TODO: Flush this out.
		NumPages:           book.Details.NumPages,
		RatingCount:        book.Stats.RatingsCount,
		RatingSum:          book.Stats.RatingsSum,
		AverageRating:      book.Stats.AverageRating,
		URL:                book.WebUrl,
		// TODO: Omitting release date is a way to essentially force R to hide
		// the book from the frontend while allowing the user to still add it
		// via search. Better UX depending on what you're after.
	}

	if book.Details.PublicationTime != 0 {
		bookRsc.ReleaseDate = releaseDate(book.Details.PublicationTime)
		bookRsc.ReleaseDateRaw = time.UnixMilli(int64(book.Details.PublicationTime)).UTC().Format(time.DateOnly)
	}

	author := book.PrimaryContributorEdge.Node
	authorDescription := strings.TrimSpace(author.Description)
	if authorDescription == "" {
		authorDescription = "N/A" // Must be set?
	}

	// Unlike bookDescription we can't request this with (stripped: true)
	authorDescription = html.UnescapeString(_stripTags.Sanitize(authorDescription))

	authorRsc := AuthorResource{
		KCA:         author.Id,
		Name:        author.Name,
		ForeignID:   author.LegacyId,
		URL:         author.WebUrl,
		ImageURL:    author.ProfileImageUrl,
		Description: authorDescription,
		Series:      series,
	}

	workRsc := workResource{
		Title:        work.BestBook.TitlePrimary,
		FullTitle:    work.BestBook.Title,
		ShortTitle:   work.BestBook.TitlePrimary,
		KCA:          work.Id,
		ForeignID:    work.LegacyId,
		URL:          work.Details.WebUrl,
		Series:       series,
		Genres:       genres,
		RelatedWorks: []int{},
		BestBookID:   work.BestBook.LegacyId,
	}

	if work.Details.PublicationTime != 0 {
		workRsc.ReleaseDate = releaseDate(work.Details.PublicationTime)
		workRsc.ReleaseDateRaw = time.UnixMilli(int64(work.Details.PublicationTime)).UTC().Format(time.DateOnly)
	} else if bookRsc.ReleaseDate != "" {
		workRsc.ReleaseDate = bookRsc.ReleaseDate
		workRsc.ReleaseDateRaw = bookRsc.ReleaseDateRaw
	}

	bookRsc.Contributors = []contributorResource{{
		ForeignID: work.BestBook.PrimaryContributorEdge.Node.LegacyId, // This might not match the edition's author, in which case we'll discard the edition.
		Role:      "Author",
	}}
	authorRsc.Works = []workResource{workRsc}
	workRsc.Authors = []AuthorResource{authorRsc}
	workRsc.Books = []bookResource{bookRsc} // TODO: Add best book here as well?

	return workRsc
}

// GetAuthor returns an author with all of their works and respective editions.
// Due to the way R works, if a work isn't returned here it's not fetchable.
//
// On an initial load we return only one work on the author. The controller
// handles asynchronously fetching all additional works.
func (g *GRGetter) GetAuthor(ctx context.Context, authorID int64) ([]byte, error) {
	var authorKCA string

	Log(ctx).Debug("getting author", "authorID", authorID)

	authorBytes, ok := g.cache.Get(ctx, AuthorKey(authorID))

	if ok {
		// Use our cached value to recover the new KCA.
		var author AuthorResource
		_ = json.Unmarshal(authorBytes, &author)
		authorKCA = author.KCA
		if authorKCA != "" {
			Log(ctx).Debug("found cached author", "authorKCA", authorKCA, "authorID", authorID)
		}
	}

	var err error
	if authorKCA == "" {
		Log(ctx).Debug("resolving author KCA", "authorID", authorID)
		authorKCA, err = g.legacyAuthorIDtoKCA(ctx, authorID)
		if err != nil {
			return nil, fmt.Errorf("resolving author: %w", err)
		}
	}

	if authorKCA == "" {
		Log(ctx).Warn("unable to resolve author KCA", "hit", ok)
		return nil, fmt.Errorf("unable to resolve author %d", authorID)
	}

	works, err := gr.GetAuthorWorks(ctx, g.gql, gr.GetWorksByContributorInput{
		Id: authorKCA,
	}, gr.PaginationInput{Limit: 20})
	if err != nil {
		Log(ctx).Warn("problem getting author works", "err", err, "author", authorID, "authorKCA", authorKCA)
		return nil, fmt.Errorf("author works: %w", err)
	}

	if len(works.GetWorksByContributor.Edges) == 0 {
		Log(ctx).Warn("no works found")
		return nil, fmt.Errorf("not found")
		// TODO: Return a 404 here instead?
	}

	// Load books until we find one with our author.
	for _, e := range works.GetWorksByContributor.Edges {
		id := e.Node.BestBook.LegacyId
		workBytes, _, _, err := g.GetBook(ctx, id, nil)
		if err != nil {
			Log(ctx).Warn("problem getting initial book for author", "err", err, "bookID", id, "authorID", authorID)
			continue
		}
		var w workResource
		err = json.Unmarshal(workBytes, &w)
		if err != nil {
			Log(ctx).Warn("problem unmarshaling work for author", "err", err, "bookID", id)
			_ = g.cache.Expire(ctx, BookKey(id))
			continue
		}

		for _, a := range w.Authors {
			if a.ForeignID != authorID {
				continue
			}
			a.Works = []workResource{w}
			return json.Marshal(a) // Found it!
		}
	}

	return nil, errNotFound
}

// GetSeries returns works belonging to the given series.
func (g *GRGetter) GetSeries(ctx context.Context, seriesID int64) (*SeriesResource, error) {
	if seriesID == 0 {
		// Not sure why this is happening.
		return nil, errors.Join(errNotFound, errors.New("series ID was 0"))
	}
	seriesRsc := &SeriesResource{
		LinkItems: []seriesWorkLinkResource{},
	}

	for page := 1; page <= 15; page++ {
		url := fmt.Sprintf("/series/show/%d?key=%s&limit=100&page=%d", seriesID, _grkey, page)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			Log(ctx).Debug("problem creating request", "err", err)
			return nil, err
		}

		resp, err := g.upstream.Do(req)
		if err != nil {
			return nil, fmt.Errorf("doing upstream: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != 200 {
			Log(ctx).Warn("problem getting series", "seriesID", seriesID, "err", err)
			return nil, fmt.Errorf("getting series %q: %w", seriesID, err)
		}

		var r struct {
			Series struct {
				Title       string `xml:"title"`
				Description string `xml:"description"`
				ID          int64  `xml:"id"`
				SeriesWorks struct {
					SeriesWork []struct {
						UserPosition string `xml:"user_position"`
						Work         struct {
							ID int64 `xml:"id"`
						} `xml:"work"`
					} `xml:"series_work"`
				} `xml:"series_works"`
			} `xml:"series"`
		}

		err = xml.NewDecoder(resp.Body).Decode(&r)
		if err != nil {
			return nil, fmt.Errorf("parsing response: %w", err)
		}

		if seriesRsc.Title == "" {
			seriesRsc.Title = strings.TrimSpace(r.Series.Title)
			seriesRsc.Description = strings.TrimSpace(r.Series.Description)
			seriesRsc.ForeignID = r.Series.ID
		}

		for idx, sw := range r.Series.SeriesWorks.SeriesWork {
			seriesRsc.LinkItems = append(seriesRsc.LinkItems, seriesWorkLinkResource{
				SeriesPosition:   100*(page-1) + idx + 1, // ??
				PositionInSeries: sw.UserPosition,
				ForeignWorkID:    sw.Work.ID,
				Primary:          false, // What is this?
			})
		}

		// Only keep going if we have a full page.
		if len(r.Series.SeriesWorks.SeriesWork) < 100 {
			break
		}
	}

	return seriesRsc, nil
}

// GetAuthorBooks enumerates all of the "best" editions for an author. This is
// how we load large authors.
func (g *GRGetter) GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq[int64] {
	authorBytes, err := g.GetAuthor(ctx, authorID)
	if err != nil {
		Log(ctx).Warn("problem getting author for full load", "err", err)
		return func(yield func(int64) bool) {} // Empty iterator.
	}

	var author AuthorResource
	_ = sonic.ConfigStd.Unmarshal(authorBytes, &author)

	return func(yield func(int64) bool) {
		after := ""
		for {
			works, err := gr.GetAuthorWorks(ctx, g.gql, gr.GetWorksByContributorInput{
				Id: author.KCA,
			}, gr.PaginationInput{Limit: 20, After: after})
			if err != nil {
				Log(ctx).Warn("problem getting author works", "err", err, "author", authorID, "authorKCA", author.KCA, "after", after)
				return
			}

			for _, w := range works.GetWorksByContributor.Edges {
				// Make sure it's actually our author and not a translator or something.
				if w.Node.BestBook.PrimaryContributorEdge.Node.LegacyId != authorID {
					continue // Wrong author.
				}
				if w.Node.BestBook.PrimaryContributorEdge.Role != "Author" {
					continue // Skip things they didn't author.
				}
				if !yield(w.Node.BestBook.LegacyId) {
					return
				}
			}

			if !works.GetWorksByContributor.PageInfo.HasNextPage {
				return
			}
			after = works.GetWorksByContributor.PageInfo.NextPageToken
		}
	}
}

// Recommendations returns the trending works on the "explore" page.
func (g *GRGetter) Recommendations(ctx context.Context, page int64) (RecommentationsResource, error) {
	if page > 1 || page < 0 {
		// GR is limitted to 50 Recommendations and doens't paginate. In the future this could be
		return RecommentationsResource{WorkIDs: []int64{}}, nil
	}
	recommended, err := gr.GetRecommended(ctx, g.gql)
	if err != nil {
		return RecommentationsResource{}, fmt.Errorf("getting recommendations: %w", err)
	}

	result := RecommentationsResource{WorkIDs: []int64{}}
	for _, e := range recommended.GetHomeWidgets.Edges {
		for _, r := range e.Node.Recommendations {
			if r.GetTypename() != "HomeWidgetWorkEdge" {
				continue
			}
			w, ok := r.GetNode().(*gr.GetRecommendedGetHomeWidgetsHomeWidgetItemsConnectionEdgesHomeWidgetEdgeNodeHomeWidgetRecommendationsEdgeNodeWork)
			if !ok {
				continue
			}
			if w.Details.WebUrl == "" {
				continue
			}
			workID, err := pathToID(w.Details.WebUrl)
			if err != nil {
				continue
			}
			result.WorkIDs = append(result.WorkIDs, workID)
		}
	}
	return result, nil
}

// legacyAuthorIDtoKCA resolves a legacy author ID to the new KCA URI. This is
// the only place where we still use the deprecated API.
func (g *GRGetter) legacyAuthorIDtoKCA(ctx context.Context, authorID int64) (string, error) {
	url := fmt.Sprintf("/author/show/%d?key=%s", authorID, _grkey)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		Log(ctx).Debug("problem creating request", "err", err)
		return "", err
	}

	resp, err := g.upstream.Do(req)
	if err != nil {
		return "", fmt.Errorf("doing upstream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var r struct {
		Author struct {
			Name  string `xml:"name"`
			Books []struct {
				Book struct {
					Authors []struct {
						Author struct {
							URI  string `xml:"uri"`
							Name string `xml:"name"`
						} `xml:"author"`
					} `xml:"authors"`
				} `xml:"book"`
			} `xml:"books"`
		} `xml:"author"`
	}

	err = xml.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}

	var kca string

	for _, b := range r.Author.Books {
		for _, a := range b.Book.Authors {
			if a.Author.Name == r.Author.Name {
				kca = strings.TrimSpace(a.Author.URI)
				break
			}
		}
	}

	Log(ctx).Debug(
		"resolved legacy author",
		"name", r.Author.Name,
		"authorID", authorID,
		"authorKCA", kca,
	)

	return kca, nil
}

// releaseDate parses a G— float into a formatted time R— can work with.
//
// TODO: We might be able to omit the month/day and have R use just the year?
func releaseDate(t float64) string {
	ts := time.UnixMilli(int64(t)).UTC()

	if ts.Before(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)) {
		return ""
	}

	if ts.After(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)) {
		return ""
	}

	return ts.Format(time.DateTime)
}

// editionDedupe is how we avoid grabbing unnecessary editions. If we've
// already seen an edition with the same title and language, then we don't need
// any more for the same title and language.
type editionDedupe struct {
	title    string
	language string
	audio    bool
}
