package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"strings"

	"github.com/Khan/genqlient/graphql"
	"github.com/blampe/rreading-glasses/hardcover"
)

// hcGetter implements a Getter using the Hardcover API as its source.
//
// Because Hardcover doesn't track GR work IDs we still need to make upstream
// HEAD requests to resolve book/work relations.
type hcGetter struct {
	cache    *layeredcache
	gql      graphql.Client
	upstream *http.Client
}

var _ getter = (*hcGetter)(nil)

func newHardcoverGetter(cache *layeredcache, gql graphql.Client, upstream *http.Client) (*hcGetter, error) {
	return &hcGetter{cache: cache, gql: gql, upstream: upstream}, nil
}

// GetWork returns the canonical edition for a book. Hardcover's GR mappings
// are entirely edition-based, with one edition representing the canonical
// book/work.
//
// A GR Work ID should therefore be mapped to a HC Book ID. However the HC API
// only allows us to query GR Book ID -> HC Edition ID. Therefore we perform a
// HEAD request to the GR work to resolve it's canonical Book ID, and then
// return that.
func (g *hcGetter) GetWork(ctx context.Context, grWorkID int64) ([]byte, int64, error) {
	if workBytes, ok := g.cache.Get(ctx, workKey(grWorkID)); ok {
		return workBytes, 0, nil
	}

	bookID, err := g.resolveRedirect(ctx, fmt.Sprintf("/work/%d", grWorkID))
	if err != nil {
		return nil, 0, fmt.Errorf("problem getting HEAD: %w", err)
	}

	log(ctx).Debug("getting book", "bookID", bookID)

	workBytes, _, authorID, err := g.GetBook(ctx, bookID)
	return workBytes, authorID, err
}

func identifiersToGR(identifiers json.RawMessage) (int64, error) {
	var grIdentifier struct {
		Goodreads []string `json:"goodreads"`
	}
	err := json.Unmarshal(identifiers, &grIdentifier)
	if err != nil {
		return 0, err
	}

	if len(grIdentifier.Goodreads) > 0 {
		return pathToID(grIdentifier.Goodreads[0])
	}

	return 0, fmt.Errorf("unable to find ID in %q", identifiers)
}

// GetBook looks up a GR book (edition) in Hardcover's mappings.
func (g *hcGetter) GetBook(ctx context.Context, grBookID int64) ([]byte, int64, int64, error) {
	if workBytes, ok := g.cache.Get(ctx, bookKey(grBookID)); ok {
		return workBytes, 0, 0, nil
	}

	grWorkID := make(chan int64)

	// Resolve the GR work ID in parallel with the HC call.
	go func() {
		// TODO: Cache this.
		defer close(grWorkID)
		workID, err := g.resolveRedirect(ctx, fmt.Sprintf("/book/editions/%d", grBookID))
		if err != nil {
			log(ctx).Warn("problem resolving workID", "err", err, "bookID", grBookID)
		}
		grWorkID <- workID
	}()

	resp, err := hardcover.GetBook(ctx, g.gql, fmt.Sprint(grBookID))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("getting book: %w", err)
	}

	if len(resp.Book_mappings) == 0 {
		return nil, 0, 0, errNotFound
	}
	bm := resp.Book_mappings[0]

	tags := []struct {
		Tag string `json:"tag"`
	}{}
	genres := []string{}

	err = json.Unmarshal(bm.Book.Cached_tags, &tags)
	if err != nil {
		return nil, 0, 0, err
	}
	for _, t := range tags {
		genres = append(genres, t.Tag)
	}
	if len(genres) == 0 {
		genres = []string{"none"}
	}

	series := []seriesResource{}
	for _, s := range bm.Book.Book_series {
		series = append(series, seriesResource{
			Title:       s.Series.Name,
			ForeignID:   s.Series.Id,
			Description: s.Series.Description,

			LinkItems: []seriesWorkLinkResource{{
				PositionInSeries: fmt.Sprint(s.Position),
				SeriesPosition:   int(s.Position), // TODO: What's the difference b/t placement?
				ForeignWorkID:    -1,              // TODO: Needs to be GR Work ID.
				Primary:          false,           // TODO: What is this?
			}},
		})
	}

	bookDescription := strings.TrimSpace(bm.Edition.Description)
	if bookDescription == "" {
		bookDescription = bm.Book.Description
	}
	if bookDescription == "" {
		bookDescription = "N/A" // Must be set.
	}

	bookRsc := bookResource{
		ForeignID:          grBookID,
		Asin:               bm.Edition.Asin,
		Description:        bookDescription,
		Isbn13:             bm.Edition.Isbn_13,
		Title:              bm.Edition.Title,
		Language:           bm.Edition.Language.Language,
		Format:             bm.Edition.Edition_format,
		EditionInformation: "",                        // TODO: Is this used anywhere?
		Publisher:          bm.Edition.Publisher.Name, // TODO: Ignore books without publishers?
		ImageURL:           strings.ReplaceAll(string(bm.Book.Cached_image), `"`, ``),
		IsEbook:            true, // TODO: Flush this out.
		NumPages:           bm.Edition.Pages,
		RatingCount:        bm.Book.Ratings_count,
		RatingSum:          int64(float64(bm.Book.Ratings_count) * bm.Book.Rating),
		AverageRating:      bm.Book.Rating,
		URL:                "https://hardcover.app/books/" + bm.Book.Slug,
		ReleaseDate:        bm.Edition.Release_date,

		// TODO: Omitting release date is a way to essentially force R to hide
		// the book from the frontend while allowing the user to still add it
		// via search. Better UX depending on what you're after.
	}

	authorDescription := "N/A" // Must be set.
	author := bm.Book.Contributions[0].Author
	if author.Bio != "" {
		authorDescription = author.Bio
	}

	grAuthorID, _ := identifiersToGR(author.Identifiers)

	authorRsc := authorResource{
		KCA:         fmt.Sprint(author.Id),
		Name:        author.Name,
		ForeignID:   grAuthorID,
		URL:         "https://hardcover.app/authors/" + author.Slug,
		ImageURL:    strings.ReplaceAll(string(author.Cached_image), `"`, ``),
		Description: authorDescription,
		Series:      series, // TODO:: Doesn't fully work yet #17.
	}

	// If we haven't already cached this author do so now, because we don't
	// normally have a way to lookup GR Author ID -> HC Author. This will get
	// incrementally filled in by ensureWork.
	if _, ok := g.cache.Get(ctx, authorKey(grAuthorID)); !ok {
		authorBytes, _ := json.Marshal(authorRsc)
		g.cache.Set(ctx, authorKey(grAuthorID), authorBytes, _authorTTL)
	}

	workRsc := workResource{
		Title:        bm.Book.Title,
		ForeignID:    <-grWorkID,
		URL:          "https://hardcover.app/books/" + bm.Book.Slug,
		ReleaseDate:  bm.Book.Release_date,
		Series:       series,
		Genres:       genres,
		RelatedWorks: []int{},
	}

	bookRsc.Contributors = []contributorResource{{ForeignID: grAuthorID, Role: "Author"}}
	authorRsc.Works = []workResource{workRsc}
	workRsc.Authors = []authorResource{authorRsc}
	workRsc.Books = []bookResource{bookRsc} // TODO: Add best book here as well?

	out, err := json.Marshal(workRsc)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("marshaling work")
	}

	// If a work isn't already cached with this ID, write one using our edition as a starting point.
	if _, ok := g.cache.Get(ctx, workKey(workRsc.ForeignID)); !ok {
		g.cache.Set(ctx, workKey(workRsc.ForeignID), out, 2*_workTTL)
	}

	return out, workRsc.ForeignID, authorRsc.ForeignID, nil
}

func (g *hcGetter) GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq[int64] {
	authorBytes, ok := g.cache.Get(ctx, authorKey(authorID))
	if !ok {
		log(ctx).Debug("skipping uncached author", "authorID", authorID)
		return nil
	}

	log(ctx).Debug("getting all works", "authorID", authorID)
	var author authorResource
	err := json.Unmarshal(authorBytes, &author)
	if err != nil {
		log(ctx).Warn("problem unmarshaling author", "authorID", authorID)
		return nil
	}

	hcAuthorID, _ := pathToID(author.KCA)

	return func(yield func(int64) bool) {
		limit, offset := int64(20), int64(0)
		for {
			gae, err := hardcover.GetAuthorEditions(ctx, g.gql, hcAuthorID, limit, offset)
			if err != nil {
				log(ctx).Warn("problem getting author editions", "err", err, "authorID", authorID)
				return
			}

			if len(gae.Authors) == 0 {
				log(ctx).Warn("expected an author but got none", "authorID", authorID)
				return
			}

			hcAuthor := gae.Authors[0]
			for _, c := range hcAuthor.Contributions {
				if len(c.Book.Book_mappings) == 0 {
					log(ctx).Debug("no mappings found")
					continue
				}

				grAuthorID, _ := pathToID(string(hcAuthor.Identifiers))
				if grAuthorID != authorID {
					log(ctx).Debug("skipping unrelated author", "want", authorID, "got", grAuthorID)
					continue
				}

				externalID := c.Book.Book_mappings[0].External_id
				grBookID, err := pathToID(externalID)
				if err != nil {
					log(ctx).Warn("unexpected ID error", "err", err, "externalID", externalID)
					continue
				}

				if !yield(grBookID) {
					return
				}
			}

			// This currently returns a ton of stuff including translated works. So we
			// stop prematurely instead of loading all of it for now.
			// offset += limit
			break
		}
	}
}

// GetAuthor looks up a GR author on Hardcover. The HC API doesn't track GR
// author IDs, so we only become aware of the HC ID once one of the author's
// books is queried in GetBook.
func (g *hcGetter) GetAuthor(ctx context.Context, grAuthorID int64) ([]byte, error) {
	authorBytes, ok := g.cache.Get(ctx, authorKey(grAuthorID))

	if !ok {
		// We don't yet have a HC author ID, so give up.
		return nil, errNotFound
	}

	// Nothing else to load for now -- works will be attached asynchronously by
	// the controller.
	return authorBytes, nil
}

// resolveRedirect performs a HEAD request against the given URL, which is
// expected to return a redirect. An ID is extracted from the location header
// and returned. For example this allows resolving a canonical book ID by
// sniffing /work/{id}.
func (g *hcGetter) resolveRedirect(ctx context.Context, url string) (int64, error) {
	head, _ := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	resp, err := g.upstream.Do(head)
	if err != nil {
		return 0, fmt.Errorf("problem getting HEAD: %w", err)
	}

	location := resp.Header.Get("location")
	if location == "" {
		return 0, fmt.Errorf("missing location header")
	}

	id, err := pathToID(location)
	if err != nil {
		log(ctx).Warn("likely auth error", "err", err, "head", url, "redirect", location)
		return 0, fmt.Errorf("invalid redirect, likely auth error: %w", err)
	}

	return id, nil
}
