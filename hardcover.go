package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

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
func (g *hcGetter) GetWork(ctx context.Context, grWorkID int64) ([]byte, error) {
	if workBytes, ok := g.cache.Get(ctx, workKey(grWorkID)); ok {
		return workBytes, nil
	}

	bookID, err := g.resolveRedirect(ctx, fmt.Sprintf("/work/%d", grWorkID))
	if err != nil {
		return nil, fmt.Errorf("problem getting HEAD: %w", err)
	}

	log(ctx).Debug("getting book", "bookID", bookID)

	return g.GetBook(ctx, bookID)
}

// GetBook looks up a GR book (edition) in Hardcover's mappings.
func (g *hcGetter) GetBook(ctx context.Context, grBookID int64) ([]byte, error) {
	if workBytes, ok := g.cache.Get(ctx, bookKey(grBookID)); ok {
		return workBytes, nil
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
		return nil, fmt.Errorf("getting book: %w", err)
	}

	if len(resp.Book_mappings) == 0 {
		return nil, errNotFound
	}
	bm := resp.Book_mappings[0]

	tags := []struct {
		Tag string `json:"tag"`
	}{}
	genres := []string{}

	err = json.Unmarshal(bm.Book.Cached_tags, &tags)
	if err != nil {
		return nil, err
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

	var authorIdentifier struct {
		Goodreads []string `json:"goodreads"`
	}
	err = json.Unmarshal(author.Identifiers, &authorIdentifier)
	if err != nil {
		return nil, err
	}

	grAuthorID := int64(-1)
	if len(authorIdentifier.Goodreads) > 0 {
		grAuthorID, _ = pathToID(authorIdentifier.Goodreads[0])
	}

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
	// normally have a way to lookup GR Author ID -> HC Author.
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
		return nil, fmt.Errorf("marshaling work")
	}
	return out, nil
}

// GetAuthor looks up a GR author on Hardcover. The HC API doesn't track GR
// author IDs, so we only become aware of the HC ID once one of the author's
// books is queried in GetBook.
func (g *hcGetter) GetAuthor(ctx context.Context, grAuthorID int64) ([]byte, error) {
	var authorKCA string

	authorBytes, ok := g.cache.Get(ctx, authorKey(grAuthorID))

	if !ok {
		// We don't yet have a HC author ID, so give up.
		return nil, errNotFound
	}

	var author authorResource
	err := json.Unmarshal(authorBytes, &author)
	if err != nil {
		return nil, err
	}

	if len(author.Works) > 0 {
		// Works were previously populated, we're done.
		return authorBytes, nil
	}

	hcAuthorID, _ := pathToID(author.KCA)
	gaw, err := hardcover.GetAuthorWorks(ctx, g.gql, hcAuthorID)
	if err != nil {
		log(ctx).Warn("problem getting author works", "err", err, "author", grAuthorID, "authorKCA", authorKCA)
		return nil, fmt.Errorf("author works: %w", err)
	}

	if len(gaw.Authors) == 0 {
		return nil, errNotFound
	}

	contributions := gaw.Authors[0].Contributions

	wg := sync.WaitGroup{}
	mu := sync.Mutex{}
	ww := []workResource{}

	// Load 20 books by default on the author. Additional books will be
	// incrementally added with time.
	for _, c := range contributions {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if len(c.Book.Book_mappings) == 0 {
				return
			}

			grBookID, _ := pathToID(c.Book.Book_mappings[0].External_id)
			workBytes, err := g.GetBook(ctx, grBookID)
			if err != nil {
				log(ctx).Warn("problem getting book for author", "err", err, "bookIO", grBookID)
				return
			}

			g.cache.Set(ctx, bookKey(grBookID), workBytes, _editionTTL)

			var w workResource
			err = json.Unmarshal(workBytes, &w)
			if err != nil {
				log(ctx).Warn("problem unmarshaling work for author", "err", err, "bookID", grBookID)
				return
			}

			mu.Lock()
			ww = append(ww, w)
			mu.Unlock()
		}()

	}

	wg.Wait()

	for _, w := range ww {
		for _, a := range w.Authors {
			if a.ForeignID != grAuthorID {
				continue
			}
			author = a
			break
		}
	}

	if author.ForeignID != grAuthorID {
		log(ctx).Warn("resolved to wrong author", "expected", grAuthorID, "got", author.ForeignID)
		return nil, fmt.Errorf("incorrectly resolved to author %d", author.ForeignID)
	}

	ratingSum := int64(0)
	ratingCount := int64(0)
	for _, w := range ww {
		ratingCount += w.Books[0].RatingCount
		ratingSum += w.Books[0].RatingSum
	}
	author.RatingCount = ratingCount
	if ratingCount != 0 {
		author.AverageRating = float32(ratingSum) / float32(ratingCount)
	}

	author.Works = ww
	author.Series = []seriesResource{}

	// Collect series and merge link items so each SeriesResource collects all
	// of the linked works.
	// TODO: Do this in the controller.
	series := map[int64]*seriesResource{}
	for _, w := range author.Works {
		for _, s := range w.Series {
			if ss, ok := series[s.ForeignID]; ok {
				ss.LinkItems = append(ss.LinkItems, s.LinkItems...)
				continue
			}
			series[s.ForeignID] = &s
		}
	}
	for _, s := range series {
		author.Series = append(author.Series, *s)
	}

	out, err := json.Marshal(author)
	if err != nil {
		return nil, fmt.Errorf("marshaling author: %w", err)
	}
	return out, nil
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
