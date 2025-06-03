package internal

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Khan/genqlient/graphql"
	q "github.com/antchfx/htmlquery"
	"github.com/blampe/rreading-glasses/gr"
	"github.com/microcosm-cc/bluemonday"
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
func NewGRGQL(ctx context.Context, upstream *http.Client, cookie string) (graphql.Client, error) {
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
	rate := time.Second // 3RPS seems to be the limit for all gql traffic, regardless of credentials.

	// This path is disabled for now because unauth'd traffic is allowed the
	// same RPS as auth'd. The value of the cookie then is to simply allow more
	// HEAD requests when resolving authors.
	/*
		if cookie != "" {
			// Grab an authenticated token and continue to refresh it in the background.
			token, err := getGRCreds(ctx, upstream)
			if err != nil {
				return nil, err
			}
			auth.Key = "Authorization"
			auth.Value = token

			go func() {
				for {
					time.Sleep(290 * time.Second) // TODO: Use cookie expiration time.
					token, err := getGRCreds(ctx, upstream)
					if err != nil {
						Log(ctx).Error("unable to refresh auth", "err", err)
						auth.Key = "X-Api-Key"
						auth.Value = string(defaultToken)
						continue
					}
					auth.Key = "Authorization"
					auth.Value = token
				}
			}()
		}
	*/

	return NewBatchedGraphQLClient(string(host), &http.Client{Transport: auth}, rate)
}

// GetWork returns a work with all known editions. Due to the way R—— works, if
// an edition is missing here (like a translated edition) it's not fetchable.
func (g *GRGetter) GetWork(ctx context.Context, workID int64, loadEditions editionsCallback) (_ []byte, authorID int64, _ error) {
	if workID == 146797269 {
		// This work always 500s for some reason. Ignore it.
		return nil, 0, errNotFound
	}
	workBytes, ttl, ok := g.cache.GetWithTTL(ctx, WorkKey(workID))
	if ok && ttl > 0 {
		return workBytes, 0, nil
	}

	if ok {
		var work workResource
		_ = json.Unmarshal(workBytes, &work)

		bookID := work.BestBookID
		if bookID != 0 {
			out, _, authorID, err := g.GetBook(ctx, bookID, loadEditions)
			return out, authorID, err
		}
	}

	url := fmt.Sprintf("/work/%d", workID)
	resp, err := g.upstream.Head(url)
	if err != nil {
		return nil, 0, fmt.Errorf("probleam getting HEAD: %w", err)
	}

	location := resp.Header.Get("location")
	if location == "" {
		return nil, 0, fmt.Errorf("missing location header")
	}

	bookID, err := pathToID(location)
	if err != nil {
		Log(ctx).Warn("likely auth error", "err", err, "head", url, "redirect", location)
		return nil, 0, fmt.Errorf("invalid redirect, likely auth error: %w", err)
	}

	Log(ctx).Debug("getting book", "bookID", bookID)

	out, _, authorID, err := g.GetBook(ctx, bookID, loadEditions)
	return out, authorID, err
}

// GetBook fetches a book (edition) from GR.
func (g *GRGetter) GetBook(ctx context.Context, bookID int64, loadEditions editionsCallback) (_ []byte, workID, authorID int64, _ error) {
	if workBytes, ttl, ok := g.cache.GetWithTTL(ctx, BookKey(bookID)); ok && ttl > 0 && loadEditions == nil {
		return workBytes, 0, 0, nil
	}

	resp, err := gr.GetBook(ctx, g.gql, bookID)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("getting book: %w", err)
	}

	book := resp.GetBookByLegacyId

	genres := []string{}
	for _, g := range book.BookGenres {
		genres = append(genres, g.Genre.Name)
	}
	if len(genres) == 0 {
		genres = []string{"none"}
	}

	series := []seriesResource{}
	for _, s := range book.BookSeries {
		legacyID, _ := pathToID(s.Series.WebUrl)
		position, _ := pathToID(s.SeriesPlacement)
		series = append(series, seriesResource{
			KCA:         s.Series.Id,
			Title:       s.Series.Title,
			ForeignID:   legacyID,
			Description: "TODO", // Would need to scrape this.

			LinkItems: []seriesWorkLinkResource{{
				PositionInSeries: s.SeriesPlacement,
				SeriesPosition:   int(position), // TODO: What's the difference b/t placement?
				ForeignWorkID:    book.Work.LegacyId,
				Primary:          false, // TODO: How can we get this???
			}},
		})
	}

	bookDescription := strings.TrimSpace(book.Description)
	if bookDescription == "" {
		bookDescription = "N/A" // Must be set?
	}

	bookRsc := bookResource{
		KCA:                resp.GetBookByLegacyId.Id,
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

	work := book.Work
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
	} else if bookRsc.ReleaseDate != "" {
		workRsc.ReleaseDate = bookRsc.ReleaseDate
	}

	bookRsc.Contributors = []contributorResource{{ForeignID: author.LegacyId, Role: "Author"}}
	authorRsc.Works = []workResource{workRsc}
	workRsc.Authors = []AuthorResource{authorRsc}
	workRsc.Books = []bookResource{bookRsc} // TODO: Add best book here as well?

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

	// If this is the "best" edition for the work, load some additional
	// editions for it.
	if loadEditions != nil && workRsc.BestBookID == bookID {
		editions := map[editionDedupe]int64{}
		for _, e := range resp.GetBookByLegacyId.Work.Editions.Edges {
			key := editionDedupe{title: strings.ToUpper(e.Node.Title), language: iso639_3(e.Node.Details.Language.Name)}
			if _, ok := editions[key]; ok {
				continue // Already saw an edition similar to this one.
			}
			editions[key] = e.Node.LegacyId
		}
		loadEditions(slices.Collect(maps.Values(editions))...)
	}

	return out, workRsc.ForeignID, authorRsc.ForeignID, nil
}

// GetAuthor returns an author with all of their works and respective editions.
// Due to the way R works, if a work isn't returned here it's not fetchable.
//
// On an initial load we return only one work on the author. The controller
// handles asynchronously fetching all additional works.
func (g *GRGetter) GetAuthor(ctx context.Context, authorID int64) ([]byte, error) {
	var authorKCA string

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
		Log(ctx).Debug("resolving author ID", "authorID", authorID)
		authorKCA, err = g.legacyAuthorIDtoKCA(ctx, authorID)
		if err != nil {
			return nil, err
		}
	}

	if authorKCA == "" {
		Log(ctx).Warn("unable to resolve author UID", "hit", ok)
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

// GetAuthorBooks enumerates all of the "best" editions for an author. This is
// how we load large authors.
func (g *GRGetter) GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq[int64] {
	authorBytes, err := g.GetAuthor(ctx, authorID)
	if err != nil {
		Log(ctx).Warn("problem getting author for full load", "err", err)
		return func(yield func(int64) bool) {} // Empty iterator.
	}

	var author AuthorResource
	_ = json.Unmarshal(authorBytes, &author)

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

// legacyAuthorIDtoKCA is the once place where we still need to hit upstream,
// because (AFAICT) the GQL APIs don't expose a way to map a legacy author ID
// to a modern kca://author ID. So we load the author's works and lookup a book
// from that, and that includes the KCA we need.
//
// We keep the author cached for longer to spare ourselves this lookup on
// refreshes.
func (g *GRGetter) legacyAuthorIDtoKCA(ctx context.Context, authorID int64) (string, error) {
	// per_page=1 is important, for some reason the default list includes works
	// by other authors!
	url := fmt.Sprintf("/author/list/%d?per_page=1", authorID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		Log(ctx).Debug("problem creating request", "err", err)
		return "", err
	}

	resp, err := g.upstream.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	// TODO: If we get a 404 for the author we should cache a gravestone.
	// Do that in the controller.

	doc, err := q.Parse(resp.Body)
	if err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}

	bookID, err := scrapeBookID(doc)
	if err != nil {
		Log(ctx).Warn("problem getting book ID", "err", err)
		return "", err
	}

	workBytes, _, _, err := g.GetBook(ctx, bookID, nil)
	if err != nil {
		Log(ctx).Warn("problem getting book for author ID lookup", "err", err, "bookID", bookID)
		return "", err
	}

	var work workResource
	err = json.Unmarshal(workBytes, &work)
	if err != nil {
		Log(ctx).Warn("problem unmarshaling book", "bookID", bookID, "size", len(workBytes))
		_ = g.cache.Expire(ctx, BookKey(bookID))
		return "", errors.Join(errTryAgain, err)
	}

	Log(ctx).Debug(
		"resolved legacy author from work",
		"workID", work.ForeignID,
		"authors", len(work.Authors),
		"authorName", work.Authors[0].Name,
		"authorID", work.Authors[0].ForeignID,
		"authorKCA", work.Authors[0].KCA,
		"title", work.Title,
	)

	return work.Authors[0].KCA, nil
}

// scrapeBookID expects `/author/list/{id}?per_page=1` as input.
func scrapeBookID(doc *html.Node) (int64, error) {
	node, err := q.Query(doc, `//a[@class="bookTitle"]`)
	if err != nil {
		return 0, fmt.Errorf("problem scraping book ID: %w", err)
	}
	if node == nil {
		return 0, fmt.Errorf("no bookTitle link found")
	}

	path := q.SelectAttr(node, "href")
	return pathToID(path)
}

// releaseDate parses a G— float into a formatted time R— can work with.
//
// TODO: We might be able to omit the month/day and have R use just the year?
func releaseDate(t float64) string {
	ts := time.UnixMilli(int64(t)).UTC()

	if ts.Before(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)) {
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
}
