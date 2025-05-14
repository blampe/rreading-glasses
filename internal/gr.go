package internal

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httputil"
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
	cache    *LayeredCache
	gql      graphql.Client
	upstream *http.Client
}

var _ getter = (*GRGetter)(nil)

// NewGRGetter creates a new Getter backed by G——R——.
func NewGRGetter(cache *LayeredCache, gql graphql.Client, upstream *http.Client) (*GRGetter, error) {
	return &GRGetter{
		cache:    cache,
		gql:      gql,
		upstream: upstream,
	}, nil
}

// GetGRCreds can be used to periodically refresh an auth token.
func GetGRCreds(ctx context.Context, upstream *http.Client) (string, error) {
	Log(ctx).Debug("generating credentials")

	getJWT, err := http.NewRequest("GET", "/open_id/auth_token", nil)
	if err != nil {
		return "", err
	}

	// This client_id is public and is easily obtainable. It's obscured here
	// only to prevent it from showing up in search results. client_id, _ =
	clientID, _ := hex.DecodeString("36336463323465376336313165366631373932663831333035386632313536306264386336393338643534612e676f6f6472656164732e636f6d")

	params := getJWT.URL.Query()
	params.Add("response_type", "token")
	params.Add("client_id", string(clientID))
	getJWT.URL.RawQuery = params.Encode()

	resp, err := upstream.Do(getJWT)
	if err != nil {
		debug, _ := httputil.DumpRequest(getJWT, true)
		Log(ctx).Error("auth error", "debug", string(debug))
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		debug, _ := httputil.DumpResponse(resp, true)
		Log(ctx).Error("auth error", "debug", string(debug))
		return "", fmt.Errorf("unexpected status %q", resp.Status)
	}
	defer func() { _ = resp.Body.Close() }()

	for _, c := range resp.Cookies() {
		if c.Name == "jwt_token" {
			return c.Value, nil
		}
	}

	return "", fmt.Errorf("no cookie found")
}

// GetWork returns a work with all known editions. Due to the way R—— works, if
// an edition is missing here (like a translated edition) it's not fetchable.
func (g *GRGetter) GetWork(ctx context.Context, workID int64) (_ []byte, authorID int64, _ error) {
	if workID == 146797269 {
		// This work always 500s for some reason. Ignore it.
		return nil, 0, errNotFound
	}
	workBytes, ttl, ok := g.cache.GetWithTTL(ctx, WorkKey(workID))
	if ok && ttl > _workTTL {
		return workBytes, 0, nil
	}

	if ok {
		var work workResource
		_ = json.Unmarshal(workBytes, &work)

		bookID := work.BestBookID
		if bookID != 0 {
			out, _, authorID, err := g.GetBook(ctx, bookID)
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

	out, _, authorID, err := g.GetBook(ctx, bookID)
	return out, authorID, err
}

// GetBook fetches a book (edition) from GR.
func (g *GRGetter) GetBook(ctx context.Context, bookID int64) (_ []byte, workID, authorID int64, _ error) {
	if workBytes, ok := g.cache.Get(ctx, BookKey(bookID)); ok {
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
		Language:           book.Details.Language.Name,
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
		g.cache.Set(ctx, WorkKey(workRsc.ForeignID), out, 2*_workTTL)
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
			Log(ctx).Debug("found cached author", "authorKCA", authorKCA)
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
		workBytes, _, _, err := g.GetBook(ctx, id)
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
	Log(ctx).Debug("extracted", "bookID", bookID)
	if err != nil {
		Log(ctx).Warn("problem getting book ID", "err", err)
		return "", err
	}

	workBytes, _, _, err := g.GetBook(ctx, bookID)
	if err != nil {
		Log(ctx).Warn("problem getting book for author ID lookup", "err", err, "bookID", bookID)
		return "", err
	}

	Log(ctx).Debug("unmarshaling", "size", len(workBytes))

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
