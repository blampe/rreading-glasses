package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Khan/genqlient/graphql"
	"github.com/blampe/rreading-glasses/hardcover"
)

// HCGetter implements a Getter using the Hardcover API as its source. It
// attempts to minimize upstream HEAD requests (to resolve book/work IDs) by
// relying on HC's raw external data.
type HCGetter struct {
	cache    cache[[]byte]
	gql      graphql.Client
	upstream *http.Client
}

var _ getter = (*HCGetter)(nil)

// NewHardcoverGetter returns a new Getter backed by Hardcover.
func NewHardcoverGetter(cache cache[[]byte], gql graphql.Client, upstream *http.Client) (*HCGetter, error) {
	return &HCGetter{cache: cache, gql: gql, upstream: upstream}, nil
}

// Search hits the GraphQL endpoint to fetch relevant work IDs and then fetches
// those in order to return the necessary edition and author IDs to the client.
func (g *HCGetter) Search(ctx context.Context, query string) ([]SearchResource, error) {
	resp, err := hardcover.Search(ctx, g.gql, query)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}
	workIDs := resp.Search.Ids

	wg := sync.WaitGroup{}
	mu := sync.Mutex{}

	results := []SearchResource{}

	for _, workID := range workIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()

			id, err := strconv.ParseInt(workID, 10, 64)
			bytes, authorID, err := g.GetWork(ctx, id, nil)

			var workRsc workResource
			err = json.Unmarshal(bytes, &workRsc)
			if err != nil {
				return
			}

			mu.Lock()
			defer mu.Unlock()

			results = append(results, SearchResource{
				BookID: workRsc.BestBookID,
				WorkID: id,
				Author: struct {
					ID int64 "json:\"id\""
				}{
					ID: authorID,
				},
			})
		}()
	}

	wg.Wait()

	return results, nil
}

// GetWork returns the canonical edition for a work.
func (g *HCGetter) GetWork(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
	workBytes, ttl, ok := g.cache.GetWithTTL(ctx, WorkKey(workID))
	if ok && ttl > 0 {
		return workBytes, 0, nil
	}

	Log(ctx).Debug("getting work", "workID", workID)

	resp, err := hardcover.GetWork(ctx, g.gql, workID)
	if err != nil {
		return nil, 0, fmt.Errorf("getting work: %w", err)
	}

	if saveEditions != nil {
		editions := map[editionDedupe]workResource{}
		for _, e := range resp.Books_by_pk.Editions {
			key := editionDedupe{
				title:    strings.ToUpper(e.EditionInfo.Title),
				language: e.EditionInfo.Language.Code3,
				audio:    e.EditionInfo.Audio_seconds != 0,
			}
			if _, ok := editions[key]; ok {
				continue // Already saw an edition similar to this one.
			}
			editions[key] = mapHardcoverToWorkResource(e.EditionInfo, resp.Books_by_pk.WorkInfo)
		}
		saveEditions(slices.Collect(maps.Values(editions))...)
	}

	workBytes, _, authorID, err := g.GetBook(ctx, resp.Books_by_pk.Default_cover_edition_id, saveEditions)
	return workBytes, authorID, err
}

// GetBook looks up a GR book (edition) in Hardcover's mappings.
func (g *HCGetter) GetBook(ctx context.Context, editionID int64, _ editionsCallback) ([]byte, int64, int64, error) {
	workBytes, ttl, ok := g.cache.GetWithTTL(ctx, BookKey(editionID))
	if ok && ttl > 0 {
		return workBytes, 0, 0, nil
	}

	Log(ctx).Debug("getting edition", "editionID", editionID)

	resp, err := hardcover.GetEdition(ctx, g.gql, editionID)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("getting book: %w", err)
	}
	work := resp.Editions_by_pk.Book.WorkInfo

	workRsc := mapHardcoverToWorkResource(resp.Editions_by_pk.EditionInfo, work)
	out, err := json.Marshal(workRsc)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("marshaling work: %w", err)
	}

	return out, workRsc.ForeignID, workRsc.Authors[0].ForeignID, nil
}

func mapHardcoverToWorkResource(edition hardcover.EditionInfo, work hardcover.WorkInfo) workResource {
	tags := []struct {
		Tag string `json:"tag"`
	}{}
	genres := []string{}

	_ = json.Unmarshal(work.Cached_tags, &tags)
	for _, t := range tags {
		genres = append(genres, t.Tag)
	}
	if len(genres) == 0 {
		genres = []string{"none"}
	}

	series := []seriesResource{}
	for _, s := range work.Book_series {
		series = append(series, seriesResource{
			Title:       s.Series.Name,
			ForeignID:   s.Series.Id,
			Description: s.Series.Description,

			LinkItems: []seriesWorkLinkResource{{
				PositionInSeries: fmt.Sprint(s.Position),
				SeriesPosition:   int(s.Position), // TODO: What's the difference b/t placement?
				ForeignWorkID:    work.Id,
				Primary:          false, // TODO: What is this?
			}},
		})
	}

	editionDescription := work.Description // edition.Description is no longer populated.
	if editionDescription == "" {
		editionDescription = "N/A" // Must be set?
	}

	editionTitle := edition.Title
	editionFullTitle := editionTitle
	editionSubtitle := edition.Subtitle

	if editionSubtitle != "" {
		editionTitle = strings.ReplaceAll(editionTitle, ": "+editionSubtitle, "")
		editionFullTitle = editionTitle + ": " + editionSubtitle
	}

	bookRsc := bookResource{
		ForeignID:   edition.Id,
		Asin:        edition.Asin,
		Description: editionDescription,
		Isbn13:      edition.Isbn_13,
		Title:       editionTitle,

		FullTitle:          editionFullTitle,
		ShortTitle:         editionTitle,
		Language:           edition.Language.Code3,
		Format:             edition.Edition_format,
		EditionInformation: edition.Edition_information, // TODO: Is this used anywhere?
		Publisher:          edition.Publisher.Name,      // TODO: Ignore books without publishers?
		ImageURL:           strings.ReplaceAll(string(work.Cached_image), `"`, ``),
		IsEbook:            edition.Edition_format == "ebook" || edition.Edition_format == "Kindle Edition",
		NumPages:           edition.Pages,
		RatingCount:        work.Ratings_count,
		RatingSum:          int64(float64(work.Ratings_count) * work.Rating),
		AverageRating:      work.Rating,
		URL:                "https://hardcover.app/books/" + work.Slug,
		ReleaseDate:        edition.Release_date,

		// TODO: Grab release date from book if absent

		// TODO: Omitting release date is a way to essentially force R to hide
		// the book from the frontend while allowing the user to still add it
		// via search. Better UX depending on what you're after.
	}

	authorDescription := "N/A" // Must be set?
	var author hardcover.WorkInfoContributionsAuthorAuthors
	if len(work.Contributions) == 0 {
		Log(context.Background()).Warn("no contribtions", "workID", work.Id, "editionID", edition.Id)
	}
	for _, c := range work.Contributions {
		if c.Author.Id != 0 {
			author = c.Author
		}
	}
	if author.AuthorInfo.Bio != "" {
		authorDescription = author.Bio
	}

	authorRsc := AuthorResource{
		Name:        author.Name,
		ForeignID:   author.Id,
		URL:         "https://hardcover.app/authors/" + author.Slug,
		ImageURL:    strings.ReplaceAll(string(author.Cached_image), `"`, ``),
		Description: authorDescription,
		Series:      series, // TODO:: Doesn't fully work yet #17.
	}

	workTitle := work.Title
	workFullTitle := workTitle
	workSubtitle := work.Subtitle

	if workSubtitle != "" {
		workTitle = strings.ReplaceAll(workTitle, ": "+workSubtitle, "")
		workFullTitle = workTitle + ": " + workSubtitle
	}

	workRsc := workResource{
		Title:        workTitle,
		FullTitle:    workFullTitle,
		ShortTitle:   workTitle,
		ForeignID:    work.Id,
		BestBookID:   work.Default_cover_edition_id,
		URL:          "https://hardcover.app/books/" + work.Slug,
		ReleaseDate:  edition.Release_date,
		Series:       series,
		Genres:       genres,
		RelatedWorks: []int{},
	}

	bookRsc.Contributors = []contributorResource{{ForeignID: author.Id, Role: "Author"}}
	authorRsc.Works = []workResource{workRsc}
	workRsc.Authors = []AuthorResource{authorRsc}
	workRsc.Books = []bookResource{bookRsc} // TODO: Add best book here as well?

	return workRsc

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
			editions, err := hardcover.GetAuthorEditions(ctx, g.gql, hcAuthorID, limit, offset)
			if err != nil {
				Log(ctx).Warn("problem getting author editions", "err", err, "authorID", authorID)
				return
			}
			if len(editions.Authors_by_pk.Contributions) == 0 {
				break
			}

			for _, c := range editions.Authors_by_pk.Contributions {
				if !yield(c.Book.Default_cover_edition_id) {
					return
				}
			}

			offset += limit
		}
	}
}

// GetAuthor looks up an author on Hardcover.
func (g *HCGetter) GetAuthor(ctx context.Context, authorID int64) ([]byte, error) {
	Log(ctx).Debug("getting author", "authorID", authorID)

	resp, err := hardcover.GetAuthorEditions(ctx, g.gql, authorID, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("getting author editions: %w", err)
	}

	if len(resp.Authors_by_pk.Contributions) == 0 {
		Log(ctx).Warn("no contributions", "authorID", authorID)
		return nil, fmt.Errorf("no contributions: %w", err)
	}

	editionID := resp.Authors_by_pk.Contributions[0].Book.Default_cover_edition_id
	workBytes, _, _, err := g.GetBook(ctx, editionID, nil)
	if err != nil {
		Log(ctx).Warn("problem getting initial book for author", "err", err, "editionID", editionID, "authorID", authorID)
		return nil, fmt.Errorf("initial edition: %w", err)
	}

	var w workResource
	err = json.Unmarshal(workBytes, &w)
	if err != nil {
		Log(ctx).Warn("problem unmarshaling work for author", "err", err, "bookID", editionID)
		_ = g.cache.Expire(ctx, BookKey(editionID))
		return nil, fmt.Errorf("unmarshaling: %w", err)
	}

	author := w.Authors[0]
	author.Works = []workResource{w}

	return json.Marshal(author)
}
