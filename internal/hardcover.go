package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/blampe/rreading-glasses/hardcover"
)

// HCGetter implements a Getter using the Hardcover API as its source. It
// attempts to minimize upstream HEAD requests (to resolve book/work IDs) by
// relying on HC's raw external data.
type HCGetter struct {
	cache cache[[]byte]
	gql   graphql.Client
}

var _ getter = (*HCGetter)(nil)

// NewHardcoverGetter returns a new Getter backed by Hardcover.
func NewHardcoverGetter(cache cache[[]byte], gql graphql.Client) (*HCGetter, error) {
	return &HCGetter{cache: cache, gql: gql}, nil
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
			if err != nil {
				Log(ctx).Warn("problem parsing", "workID", workID, "err", err)
				return
			}

			bytes, _, err := g.GetWork(ctx, id, nil)
			if err != nil {
				return
			}

			var workRsc workResource
			err = json.Unmarshal(bytes, &workRsc)
			if err != nil {
				return
			}

			if len(workRsc.Authors) == 0 {
				Log(ctx).Warn("work is missing an author", "workID", id, "err", err)
				return
			}

			mu.Lock()
			defer mu.Unlock()

			results = append(results, SearchResource{
				BookID: workRsc.BestBookID,
				WorkID: id,
				Author: SearchResourceAuthor{
					ID: workRsc.Authors[0].ForeignID,
				},
			})
		}()
	}

	wg.Wait()

	return results, nil
}

// GetWork returns the canonical edition for a work.
func (g *HCGetter) GetWork(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
	if workID == 0 {
		return nil, 0, errors.Join(errBadRequest, errors.New("work ID missing"))
	}

	workBytes, ttl, ok := g.cache.GetWithTTL(ctx, WorkKey(workID))
	if ok && ttl > 0 {
		return workBytes, 0, nil
	}

	Log(ctx).Debug("getting work", "workID", workID)

	resp, err := hardcover.GetWork(ctx, g.gql, workID)
	if err != nil {
		return nil, 0, fmt.Errorf("getting work: %w", err)
	}

	if resp.Books_by_pk.WorkInfo.Id == 0 {
		return nil, 0, errors.Join(errNotFound, fmt.Errorf("invalid work info"))
	}

	if resp.Books_by_pk.WorkInfo.State == "duplicate" {
		return g.GetWork(ctx, resp.Books_by_pk.Canonical_id, saveEditions)
	}

	if resp.Books_by_pk.WorkInfo.State == "pending" {
		return nil, 0, errors.Join(errNotFound, fmt.Errorf("book is pending"))
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

			work, err := mapHardcoverToWorkResource(ctx, e.EditionInfo, resp.Books_by_pk.WorkInfo)
			if err != nil {
				continue
			}
			editions[key] = work
		}
		saveEditions(slices.Collect(maps.Values(editions))...)
	}

	author, err := bestAuthor(hardcover.AsContributions(resp.Books_by_pk.WorkInfo.Contributions))
	if err != nil {
		return nil, 0, err
	}
	authorID := author.Id

	editionID := bestHardcoverEdition(resp.Books_by_pk.DefaultEditions, authorID)
	workBytes, _, authorID, err = g.GetBook(ctx, editionID, saveEditions)
	return workBytes, authorID, err
}

// GetBook looks up a GR book (edition) in Hardcover's mappings.
func (g *HCGetter) GetBook(ctx context.Context, editionID int64, _ editionsCallback) ([]byte, int64, int64, error) {
	if editionID == 0 {
		return nil, 0, 0, errors.Join(errBadRequest, errors.New("edition missing ID"))
	}

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

	if work.Id == 0 {
		return nil, 0, 0, errors.Join(errNotFound, fmt.Errorf("edition without work info"))
	}

	workRsc, err := mapHardcoverToWorkResource(ctx, resp.Editions_by_pk.EditionInfo, work)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("mapping for book: %w", err)
	}
	out, err := json.Marshal(workRsc)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("marshaling work: %w", err)
	}

	if len(workRsc.Authors) == 0 {
		Log(ctx).Warn("missing author", "editionID", editionID)
		return nil, 0, 0, errors.Join(errNotFound, errors.New("missing author"))
	}

	return out, workRsc.ForeignID, workRsc.Authors[0].ForeignID, nil
}

func mapHardcoverToWorkResource(ctx context.Context, edition hardcover.EditionInfo, work hardcover.WorkInfo) (workResource, error) {
	if edition.Id == 0 || work.Id == 0 {
		return workResource{}, errors.Join(errBadRequest, errors.New("missing ID"))
	}

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

	series := []SeriesResource{}
	for _, s := range work.Book_series {
		series = append(series, SeriesResource{
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
		ReleaseDate:        hcReleaseDate(edition.Release_date),

		// TODO: Grab release date from book if absent

		// TODO: Omitting release date is a way to essentially force R to hide
		// the book from the frontend while allowing the user to still add it
		// via search. Better UX depending on what you're after.
	}

	author, err := bestAuthor(hardcover.AsContributions(work.Contributions))
	if err != nil {
		return workResource{}, err
	}

	authorDescription := "N/A" // Must be set?
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
		BestBookID:   bestHardcoverEdition(work.DefaultEditions, author.Id),
		URL:          "https://hardcover.app/books/" + work.Slug,
		ReleaseDate:  hcReleaseDate(edition.Release_date),
		Series:       series,
		Genres:       genres,
		RelatedWorks: []int{},

		RatingCount:   work.Ratings_count,
		RatingSum:     int64(float64(work.Ratings_count) * work.Rating),
		AverageRating: work.Rating,
	}

	bookRsc.Contributors = []contributorResource{{ForeignID: author.Id, Role: "Author"}}
	authorRsc.Works = []workResource{workRsc}
	workRsc.Authors = []AuthorResource{authorRsc}
	workRsc.Books = []bookResource{bookRsc} // TODO: Add best book here as well?

	return workRsc, nil
}

// GetAuthorBooks returns all GR book (edition) IDs.
func (g *HCGetter) GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq[int64] {
	return func(yield func(int64) bool) {
		limit, offset := int64(100), int64(0)
		for {
			editions, err := hardcover.GetAuthorEditions(ctx, g.gql, authorID, limit, offset)
			if err != nil {
				Log(ctx).Warn("problem getting author editions", "err", err, "authorID", authorID)
				return
			}

			if len(editions.Authors_by_pk.Contributions) == 0 {
				break // All done.
			}

			for _, c := range editions.Authors_by_pk.Contributions {
				author, err := bestAuthor(hardcover.AsContributions(c.Book.Contributions))
				if err != nil {
					continue
				}
				if author.Id != authorID {
					continue // Ignore anything that doesn't have this as the primary author.
				}

				editionID := bestHardcoverEdition(c.Book.DefaultEditions, authorID)
				if editionID == 0 {
					continue // Shouldn't happen.
				}
				if !yield(editionID) {
					return
				}
			}

			offset += limit
		}
	}
}

// Recommendations returns trending work IDs from the past week.
func (g *HCGetter) Recommendations(ctx context.Context, page int64) (RecommentationsResource, error) {
	now := time.Now()
	lastWeek := now.Add(-7 * 24 * time.Hour)
	if page < 1 {
		return RecommentationsResource{}, fmt.Errorf("page must be gte 1")
	}

	recommended, err := hardcover.GetRecommended(ctx, g.gql, lastWeek.String(), now.String(), 100, 100*(page-1))
	if err != nil {
		return RecommentationsResource{}, fmt.Errorf("getting recommended: %w", err)
	}

	return RecommentationsResource{WorkIDs: recommended.Books_trending.WorkIDs}, nil
}

func bestHardcoverEdition(defaults hardcover.DefaultEditions, expectedAuthorID int64) int64 {
	author, err := bestAuthor(hardcover.AsContributions(defaults.Contributions))
	if err != nil {
		Log(context.TODO()).Warn("no author", "workID", defaults.Id)
		return 0
	}
	if expectedAuthorID != 0 && expectedAuthorID != author.Id {
		Log(context.TODO()).Warn("author mismatch", "expected", expectedAuthorID, "got", author.Id, "workID", defaults.Id)
		return 0
	}

	cover := defaults.Default_cover_edition
	if cover.Id != 0 {
		coverAuthor, _ := bestAuthor(hardcover.AsContributions(cover.Contributions))
		if coverAuthor.Id == author.Id {
			return cover.Id
		}
	}

	ebook := defaults.Default_ebook_edition
	if ebook.Id != 0 {
		ebookAuthor, _ := bestAuthor(hardcover.AsContributions(ebook.Contributions))
		if ebookAuthor.Id == author.Id {
			return ebook.Id
		}
	}

	audio := defaults.Default_cover_edition
	if audio.Id != 0 {
		audioAuthor, _ := bestAuthor(hardcover.AsContributions(audio.Contributions))
		if audioAuthor.Id == author.Id {
			return audio.Id
		}
	}

	physical := defaults.Default_physical_edition
	if physical.Id != 0 {
		physicalAuthor, _ := bestAuthor(hardcover.AsContributions(physical.Contributions))
		if physicalAuthor.Id == author.Id {
			return physical.Id
		}
	}

	if len(defaults.Fallback) == 0 {
		Log(context.TODO()).Warn("no editions", "workID", defaults.Id)
		return 0
	}

	if len(defaults.Fallback) > 1 {
		Log(context.TODO()).Warn("ambiguous editions", "workID", defaults.Id)
		return 0
	}

	return defaults.Fallback[0].Id
}

func bestAuthor(contributions []hardcover.Contributions) (hardcover.ContributionsAuthorAuthors, error) {
	if len(contributions) == 0 {
		return hardcover.ContributionsAuthorAuthors{}, errors.Join(errNotFound, fmt.Errorf("no contributions"))
	}
	for _, c := range contributions {
		switch strings.ToLower(c.Contribution) {
		// This field seems unstructured...
		case "pseudonym",
			"translator",
			"narrator", "reading",
			"adaptation",
			"illustrator", "illustrations", "ilustrator",
			"contributor & illustrator",
			"writer/illustrator", "writer, illustrator", "writer, editor",
			"brand",
			"visual art",
			"character design",
			"artist",
			"cover", "cover art", "cover artist",
			"text", "writer", "writer, editior", "author & editor", // keep?
			"penciler", "penciller", "inker", "colourist", "letterer", "colorist",
			"contributor", "contributer",
			"guion", "dibujo",
			"foreword", "foreward", "introduction", "introduction/contributor",
			"editor/introduction", "editor", "editor and contributor", "editor/contributor", "editor / contributor", "editor,contributor":
			continue
		case "", "author":
			// "Primary" authors seem to almost never have this set.
			return c.Author, nil
		default:
			continue
		}
	}
	return hardcover.ContributionsAuthorAuthors{}, errors.Join(errNotFound, fmt.Errorf("no valid contribution"))
}

// GetAuthor looks up an author on Hardcover.
func (g *HCGetter) GetAuthor(ctx context.Context, authorID int64) ([]byte, error) {
	Log(ctx).Debug("getting author", "authorID", authorID)

	if authorID == 0 {
		return nil, errors.Join(errBadRequest, errors.New("author ID missing"))
	}

	resp, err := hardcover.GetAuthorEditions(ctx, g.gql, authorID, 20, 0)
	if err != nil {
		return nil, fmt.Errorf("getting author editions: %w", err)
	}

	if resp.Authors_by_pk.AuthorInfo.Id == 0 {
		return nil, errors.Join(errNotFound, fmt.Errorf("invalid author editions"))
	}

	author, err := bestAuthor(hardcover.AsContributions(resp.Authors_by_pk.Contributions))
	if err != nil {
		return nil, err
	}
	if author.Id != authorID {
		Log(ctx).Warn("author mismatch, possibly merged?", "expected", authorID, "got", author.Id)
		return nil, errors.Join(errNotFound, fmt.Errorf("author mismatch"))
	}

	for _, cc := range resp.Authors_by_pk.Contributions {
		editionID := bestHardcoverEdition(cc.Book.DefaultEditions, authorID)
		if editionID == 0 {
			continue
		}
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

	Log(ctx).Warn("no valid works found", "authorID", authorID)
	return nil, errors.Join(errNotFound, fmt.Errorf("no valid works found"))
}

// GetSeries isn't implemented yet.
func (g *HCGetter) GetSeries(ctx context.Context, seriesID int64) (*SeriesResource, error) {
	seriesRsc := &SeriesResource{
		LinkItems: []seriesWorkLinkResource{},
	}

	limit, offset := int64(1000), int64(0)

	var lastPosition float32

	// Max out at 3k for the series.
	for offset < 3*limit {
		series, err := hardcover.GetSeries(ctx, g.gql, seriesID, limit, offset)
		if err != nil {
			return nil, fmt.Errorf("getting series %q: %w", seriesID, err)
		}

		seriesRsc.Title = series.Series_by_pk.Name
		seriesRsc.Description = series.Series_by_pk.Description
		seriesRsc.ForeignID = series.Series_by_pk.Id

		if len(series.Series_by_pk.Book_series) == 0 {
			break
		}

		for _, bs := range series.Series_by_pk.Book_series {
			if lastPosition > 0 && bs.Position == lastPosition {
				// Skip less popular duplicates.
				continue
			}
			seriesRsc.LinkItems = append(seriesRsc.LinkItems, seriesWorkLinkResource{
				ForeignWorkID:    bs.Book_id,
				PositionInSeries: bs.Details,
				SeriesPosition:   int(bs.Position),
				Primary:          bs.Featured,
			})
			lastPosition = bs.Position
		}

		if len(seriesRsc.LinkItems) >= int(series.Series_by_pk.Books_count) {
			break
		}

		offset += limit
	}

	return seriesRsc, nil
}

func hcReleaseDate(d string) string {
	if strings.HasSuffix(d, "BC") {
		return "0001-01-01"
	}
	return d
}
