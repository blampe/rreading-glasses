package internal

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

// HCGetter implements a Getter using the Hardcover API as its source. It
// attempts to minimize upstread HEAD requests (to resolve book/work IDs) by
// relying on HC's raw external data.
type HCGetter struct {
	cache    *LayeredCache
	gql      graphql.Client
	upstream *http.Client
}

var _ getter = (*HCGetter)(nil)

// NewHardcoverGetter returns a new Getter backed by Hardcover.
func NewHardcoverGetter(cache *LayeredCache, gql graphql.Client, upstream *http.Client) (*HCGetter, error) {
	return &HCGetter{cache: cache, gql: gql, upstream: upstream}, nil
}

// GetWork returns the canonical edition for a book. Hardcover's GR mappings
// are entirely edition-based, with one edition representing the canonical
// book/work.
//
// A GR Work ID should therefore be mapped to a HC Book ID. However the HC API
// only allows us to query GR Book ID -> HC Edition ID. Therefore we perform a
// HEAD request to the GR work to resolve it's canonical Book ID, and then
// return that.
func (g *HCGetter) GetWork(ctx context.Context, grWorkID int64) ([]byte, int64, error) {
	workBytes, ttl, ok := g.cache.GetWithTTL(ctx, WorkKey(grWorkID))
	if ok && ttl > 0 {
		return workBytes, 0, nil
	}

	// TODO: Loading the best book ID on a cache refresh will lose any other
	// editions previously attached to this work. Instead we should re-assemble
	// the book array by re-fetching the latest books from the cache.
	if ok {
		var work workResource
		_ = json.Unmarshal(workBytes, &work)

		bookID := work.BestBookID
		if bookID != 0 {
			out, _, authorID, err := g.GetBook(ctx, bookID)
			return out, authorID, err
		}
	}
	Log(ctx).Debug("getting work", "grWorkID", grWorkID)

	// Sniff GR to resolve the work ID.
	bookID, err := g.resolveRedirect(ctx, fmt.Sprintf("/work/%d", grWorkID))
	if err != nil {
		return nil, 0, fmt.Errorf("problem getting HEAD: %w", err)
	}

	workBytes, _, authorID, err := g.GetBook(ctx, bookID)
	return workBytes, authorID, err
}

// GetBook looks up a GR book (edition) in Hardcover's mappings.
func (g *HCGetter) GetBook(ctx context.Context, grBookID int64) ([]byte, int64, int64, error) {
	if workBytes, ok := g.cache.Get(ctx, BookKey(grBookID)); ok {
		return workBytes, 0, 0, nil
	}

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

	editionTitle := bm.Edition.Title
	editionFullTitle := editionTitle
	editionSubtitle := bm.Edition.Subtitle

	if editionSubtitle != "" {
		editionTitle = strings.ReplaceAll(editionTitle, ": "+editionSubtitle, "")
		editionFullTitle = editionTitle + ": " + editionSubtitle
	}

	bookRsc := bookResource{
		ForeignID:          grBookID,
		Asin:               bm.Edition.Asin,
		Description:        bookDescription,
		Isbn13:             bm.Edition.Isbn_13,
		Title:              editionTitle,
		FullTitle:          editionFullTitle,
		ShortTitle:         editionTitle,
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

		// TODO: Grab release date from book if absent

		// TODO: Omitting release date is a way to essentially force R to hide
		// the book from the frontend while allowing the user to still add it
		// via search. Better UX depending on what you're after.
	}

	authorDescription := "N/A" // Must be set.
	author := bm.Book.Contributions[0].Author
	if author.Bio != "" {
		authorDescription = author.Bio
	}

	workID := int64(0)
	grAuthorID := int64(0)
	for _, bmbm := range bm.Book.Book_mappings {
		var dto struct {
			RawData struct {
				Work struct {
					ID int64 `json:"id"`
				} `json:"work"`
				Authors struct {
					Author struct {
						ID string `json:"id"`
					} `json:"author"`
				} `json:"authors"`
			} `json:"raw_data"`
		}
		err := json.Unmarshal(bmbm.Dto_external, &dto)
		if err != nil {
			continue
		}
		if dto.RawData.Work.ID != 0 {
			workID = dto.RawData.Work.ID
		}
		if dto.RawData.Authors.Author.ID != "" {
			grAuthorID, _ = pathToID(dto.RawData.Authors.Author.ID)
		}
		if workID != 0 && grAuthorID != 0 {
			break
		}
	}
	if workID == 0 {
		Log(ctx).Warn("upstream doesn't have a work ID", "grBookID", grBookID)
		return nil, 0, 0, errNotFound
	}
	if grAuthorID == 0 {
		Log(ctx).Warn("upstream doesn't have an author ID", "grBookID", grBookID)
		return nil, 0, 0, errNotFound
	}

	authorRsc := AuthorResource{
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
	if _, ok := g.cache.Get(ctx, AuthorKey(grAuthorID)); !ok {
		authorBytes, _ := json.Marshal(authorRsc)
		g.cache.Set(ctx, AuthorKey(grAuthorID), authorBytes, _authorTTL)
		// Don't use 2x TTL so the next fetch triggers a refresh
	}

	workTitle := bm.Book.Title
	workFullTitle := workTitle
	workSubtitle := bm.Book.Subtitle

	if workSubtitle != "" {
		workTitle = strings.ReplaceAll(workTitle, ": "+workSubtitle, "")
		workFullTitle = workTitle + ": " + workSubtitle
	}

	workRsc := workResource{
		Title:        workTitle,
		FullTitle:    workFullTitle,
		ShortTitle:   workTitle,
		ForeignID:    workID,
		URL:          "https://hardcover.app/books/" + bm.Book.Slug,
		ReleaseDate:  bm.Book.Release_date,
		Series:       series,
		Genres:       genres,
		RelatedWorks: []int{},
	}

	bookRsc.Contributors = []contributorResource{{ForeignID: grAuthorID, Role: "Author"}}
	authorRsc.Works = []workResource{workRsc}
	workRsc.Authors = []AuthorResource{authorRsc}
	workRsc.Books = []bookResource{bookRsc} // TODO: Add best book here as well?

	out, err := json.Marshal(workRsc)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("marshaling work")
	}

	// If a work isn't already cached with this ID, write one using our edition as a starting point.
	if _, ok := g.cache.Get(ctx, WorkKey(workRsc.ForeignID)); !ok {
		g.cache.Set(ctx, WorkKey(workRsc.ForeignID), out, _workTTL)
	}

	return out, workRsc.ForeignID, authorRsc.ForeignID, nil
}

// GetAuthorBooks returns all GR book (edition) IDs.
func (g *HCGetter) GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq[int64] {
	noop := func(yield func(int64) bool) {}
	authorBytes, ok := g.cache.Get(ctx, AuthorKey(authorID))
	if !ok {
		Log(ctx).Debug("skipping uncached author", "authorID", authorID)
		return noop
	}

	var author AuthorResource
	err := json.Unmarshal(authorBytes, &author)
	if err != nil {
		Log(ctx).Warn("problem unmarshaling author", "authorID", authorID)
		return noop
	}

	hcAuthorID, _ := pathToID(author.KCA)

	return func(yield func(int64) bool) {
		limit, offset := int64(20), int64(0)
		for {
			gae, err := hardcover.GetAuthorEditions(ctx, g.gql, hcAuthorID, limit, offset)
			if err != nil {
				Log(ctx).Warn("problem getting author editions", "err", err, "authorID", authorID)
				return
			}

			if len(gae.Authors) == 0 {
				Log(ctx).Warn("expected an author but got none", "authorID", authorID)
				return
			}

			hcAuthor := gae.Authors[0]
			for _, c := range hcAuthor.Contributions {
				if len(c.Book.Book_mappings) == 0 {
					Log(ctx).Debug("no mappings found")
					continue
				}

				grAuthorID, _ := pathToID(string(hcAuthor.Identifiers))
				if grAuthorID != authorID {
					Log(ctx).Debug("skipping unrelated author", "want", authorID, "got", grAuthorID)
					continue
				}

				externalID := c.Book.Book_mappings[0].External_id
				grBookID, err := pathToID(externalID)
				if err != nil {
					Log(ctx).Warn("unexpected ID error", "err", err, "externalID", externalID)
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
func (g *HCGetter) GetAuthor(ctx context.Context, grAuthorID int64) ([]byte, error) {
	authorBytes, ok := g.cache.Get(ctx, AuthorKey(grAuthorID))

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
func (g *HCGetter) resolveRedirect(ctx context.Context, url string) (int64, error) {
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
		Log(ctx).Warn("likely auth error", "err", err, "head", url, "redirect", location)
		return 0, fmt.Errorf("invalid redirect, likely auth error: %w", err)
	}

	return id, nil
}
