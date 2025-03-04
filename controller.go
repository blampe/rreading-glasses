package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

// Use lower values while we're beta testing.
var (
	_authorTTL = 7 * 24 * time.Hour // 7 days.
	// _authorTTL  = 30 * 24 * time.Hour     // 1 month.

	_workTTL = 14 * 24 * time.Hour // 2 weeks.
	// _workTTL    = 30 * 24 * time.Hour     // 1 month.

	_editionTTL = 28 * 24 * time.Hour // 1 month.
	// _editionTTL = 6 * 30 * 24 * time.Hour // 6 months.

	// _missing is a sentinel value we cache for 404 responses.
	_missing = []byte{0}
)

// controller facilitates operations on our cache by scheduling background work
// and handling cache invalidation.
//
// Most operations take place inside a singleflight group to prevent redundant
// work.
//
// The request path is limitted to Get methods which at worst perform only O(1)
// lookups. More expensive work, like denormalization, is handled in the
// background. The original metadata server likely does this work in the
// request path, hence why larger authors don't work -- it can't complete
// O(work * editions) within the lifespan of the request.
//
// Another significant difference is that we cache data eagerly, when it is
// requested. We don't require a full database dump, so we're able to grab new
// works as soon as they're available.
type controller struct {
	cache  *layeredcache
	getter getter             // Core GetBook/GetAuthor/GetWork implementation.
	group  singleflight.Group // Coalesce lookups for the same key.

	ensureC chan edge      // Serializes edge updates.
	ensureG errgroup.Group // Limits how many authors/works we sync in the background.

	// cf       *cloudflare.API // TODO: CDN invalidation.
}

// getter allows alternative implementations of the core logic to be injected.
// Don't write to the cache if you use it.
type getter interface {
	GetWork(ctx context.Context, workID int64) (_ []byte, authorID int64, _ error)
	GetBook(ctx context.Context, bookID int64) (_ []byte, workID int64, authorID int64, _ error) // Returns a serialized Work??
	GetAuthor(ctx context.Context, authorID int64) ([]byte, error)
	GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq[int64] // Returns book/edition IDs, not works.
}

// newUpstream creates a new http.Client with middleware appropriate for use
// with an upstream.
func newUpstream(host string, cookie string, proxy string) (*http.Client, error) {
	upstream := &http.Client{
		Transport: throttledTransport{
			// TODO: Unauthenticated defaults to 1 request per minute.
			Limiter: rate.NewLimiter(rate.Every(time.Hour/60), 1),
			RoundTripper: scopedTransport{
				host:         host,
				RoundTripper: errorProxyTransport{http.DefaultTransport},
			},
		},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			// Don't follow redirects on HEAD requests. We use this to sniff
			// work->book mappings without loading everything.
			if req.Method == http.MethodHead {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	if cookie != "" {
		cookies, err := http.ParseCookie(cookie)
		if err != nil {
			return nil, fmt.Errorf("invalid cookie: %w", err)
		}
		upstream.Transport = throttledTransport{
			// Authenticated requests get a more generous 1RPS.
			Limiter: rate.NewLimiter(rate.Every(time.Second/1), 1),
			RoundTripper: scopedTransport{
				host: host,
				RoundTripper: cookieTransport{
					cookies:      cookies,
					RoundTripper: errorProxyTransport{http.DefaultTransport},
				},
			},
		}
	}
	if proxy != "" {
		url, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		// TODO: This doesn't work.
		upstream.Transport.(*http.Transport).Proxy = http.ProxyURL(url)
	}

	return upstream, nil
}

func newController(cache *layeredcache, getter getter) (*controller, error) {
	c := &controller{
		cache:  cache,
		getter: getter,

		ensureC: make(chan edge),
	}

	c.ensureG.SetLimit(10)

	return c, nil
}

// TODO: This should only return a book!
func (c *controller) GetBook(ctx context.Context, bookID int64) ([]byte, error) {
	out, err, _ := c.group.Do(bookKey(bookID), func() (any, error) {
		return c.getBook(ctx, bookID)
	})
	return out.([]byte), err
}

func (c *controller) GetWork(ctx context.Context, workID int64) ([]byte, error) {
	out, err, _ := c.group.Do(workKey(workID), func() (any, error) {
		return c.getWork(ctx, workID)
	})
	return out.([]byte), err
}

func (c *controller) GetAuthor(ctx context.Context, authorID int64) ([]byte, error) {
	out, err, _ := c.group.Do(authorKey(authorID), func() (any, error) {
		return c.getAuthor(ctx, authorID)
	})
	return out.([]byte), err
}

func (c *controller) getBook(ctx context.Context, bookID int64) ([]byte, error) {
	workBytes, ok := c.cache.Get(ctx, bookKey(bookID))
	if slices.Equal(workBytes, _missing) {
		return nil, errNotFound
	}
	if ok {
		return workBytes, nil
	}

	// Cache miss.
	workBytes, workID, _, err := c.getter.GetBook(ctx, bookID)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, bookKey(bookID), _missing, _editionTTL)
		return nil, err
	}
	if err != nil {
		log(ctx).Warn("problem getting book", "err", err, "bookID", bookID)
		return nil, err
	}

	c.cache.Set(ctx, bookKey(bookID), workBytes, _editionTTL)

	if workID > 0 {
		// Ensure the edition/book is included with the work, but don't block the response.
		go func() {
			c.ensureG.Go(func() error {
				ctx := context.Background()

				defer func() {
					if r := recover(); r != nil {
						log(ctx).Error("panic", "details", r)
					}
				}()

				c.ensureC <- edge{kind: workEdge, parentID: workID, childIDs: []int64{bookID}}

				return nil
			})
		}()
	}

	return workBytes, nil
}

func (c *controller) getWork(ctx context.Context, workID int64) ([]byte, error) {
	cachedBytes, ttl, ok := c.cache.GetWithTTL(ctx, workKey(workID))
	if slices.Equal(cachedBytes, _missing) {
		return nil, errNotFound
	}
	if ok && ttl > _workTTL {
		return cachedBytes, nil
	}

	// Cache miss.
	workBytes, authorID, err := c.getter.GetWork(ctx, workID)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, workKey(workID), _missing, _workTTL)
		return nil, err
	}
	if err != nil {
		log(ctx).Warn("problem getting work", "err", err, "workID", workID)
		return nil, err
	}

	c.cache.Set(ctx, workKey(workID), workBytes, 2*_workTTL)

	// Ensuring relationships doesn't block.
	go func() {
		c.ensureG.Go(func() error {
			ctx := context.Background()

			defer func() {
				if r := recover(); r != nil {
					log(ctx).Error("panic", "details", r)
				}
			}()

			// Ensure we keep whatever editions we already had cached.
			var cached workResource
			_ = json.Unmarshal(cachedBytes, &cached)

			cachedBookIDs := []int64{}
			for _, b := range cached.Books {
				cachedBookIDs = append(cachedBookIDs, b.ForeignID)
			}
			c.ensureC <- edge{kind: workEdge, parentID: workID, childIDs: cachedBookIDs}

			if authorID > 0 {
				// Ensure the work belongs to its author.
				c.ensureC <- edge{kind: authorEdge, parentID: authorID, childIDs: []int64{workID}}
			}

			return nil
		})
	}()

	return workBytes, err
}

// getAuthor returns an AuthorResource with up to 20 works populated. We
// persist data locally for 2x the TTL we give the client because stale data
// can be used to speed up subsequent cache misses / refreshes.
//
// NB: Author endpoints appear to have different rate limiting compared to
// works, YMMV.
func (c *controller) getAuthor(ctx context.Context, authorID int64) ([]byte, error) {
	log(ctx).Debug("looking for author", "id", authorID)

	cachedBytes, ttl, ok := c.cache.GetWithTTL(ctx, authorKey(authorID))
	if slices.Equal(cachedBytes, _missing) {
		return nil, errNotFound
	}
	// Our local TTL is 2*_authorTTL, but we want the client to see a miss
	// after _authorTTL.
	if ok && ttl > _authorTTL {
		return cachedBytes, nil
	}

	// Cache miss.
	authorBytes, err := c.getter.GetAuthor(ctx, authorID)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, authorKey(authorID), _missing, _authorTTL)
		return nil, err
	}
	if err != nil {
		log(ctx).Warn("problem getting author", "err", err, "authorID", authorID)
		return nil, err
	}

	c.cache.Set(ctx, authorKey(authorID), authorBytes, 2*_authorTTL)

	// Ensuring relationships doesn't block.
	go func() {
		c.ensureG.Go(func() error {
			ctx := context.Background()

			defer func() {
				if r := recover(); r != nil {
					log(ctx).Error("panic", "details", r)
				}
			}()

			// Ensure we keep whatever works we already had cached.
			var cached authorResource
			_ = json.Unmarshal(cachedBytes, &cached)

			workIDsToEnsure := []int64{}
			for _, w := range cached.Works {
				workIDsToEnsure = append(workIDsToEnsure, w.ForeignID)
			}

			// Finally try to load all of the author's works to ensure we have them.
			n := 0
			log(ctx).Info("fetching all works for author", "authorID", authorID)
			for bookID := range c.getter.GetAuthorBooks(context.Background(), authorID) {
				if n > 1000 {
					break
				}
				bookBytes, workID, _, err := c.getter.GetBook(ctx, bookID)
				if err != nil {
					log(ctx).Warn("problem getting book for author", "authorID", authorID, "bookID", bookID)
					continue
				}
				if workID == 0 {
					var w workResource
					_ = json.Unmarshal(bookBytes, &w)
					workID = w.ForeignID
				}
				workIDsToEnsure = append(workIDsToEnsure, workID)
				n++
			}

			slices.Sort(workIDsToEnsure)
			workIDsToEnsure = slices.Compact(workIDsToEnsure)

			c.ensureC <- edge{kind: authorEdge, parentID: authorID, childIDs: workIDsToEnsure}

			return nil
		})
	}()

	return authorBytes, nil
}

// Run is responsible for denormalizing data. Race conditions are still
// possible but less likely by serializing updates this way.
func (c *controller) Run(ctx context.Context) {
	for edge := range c.ensureC {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		switch edge.kind {
		case authorEdge:
			if err := c.ensureWorks(ctx, edge.parentID, edge.childIDs...); err != nil {
				log(ctx).Warn("problem ensuring work", "err", err, "authorID", edge.parentID, "workIDs", edge.childIDs)
			}
		case workEdge:
			if err := c.ensureEditions(ctx, edge.parentID, edge.childIDs...); err != nil {
				log(ctx).Warn("problem ensuring edition", "err", err, "workID", edge.parentID, "bookIDs", edge.childIDs)
			}
		}
		cancel()
	}
}

// Shutdown waits for all "ensure" goroutines to finish submitting their work
// and then closes the ensure channel. Run will run to completion after
// Shutdown is called.
func (c *controller) Shutdown(ctx context.Context) {
	_ = c.ensureG.Wait()
	close(c.ensureC)
}

// ensureEditions ensures that the given editions exists on the work. It
// deserializes the target work once. (TODO: No-op if it includes an edition
// with the same title).
//
// This is what allows us to support translated editions. We intentionally
// don't add every edition available, because then the user has potentially
// hundreds (or thousands!) of editions to crawl through in order to find one
// in the language they need.
//
// Instead, we (a) only add editions that users actually search for and use,
// (b) only add editions that are meaningful enough to appear in auto_complete,
// and (c) keep the total number of editions small enough for users to more
// easily select from.
func (c *controller) ensureEditions(ctx context.Context, workID int64, bookIDs ...int64) error {
	if len(bookIDs) == 0 {
		return nil
	}

	workBytes, _, err := c.getter.GetWork(ctx, workID)
	if err != nil {
		log(ctx).Debug("problem getting work", "err", err)
		return err
	}
	var work workResource
	err = json.Unmarshal(workBytes, &work)
	if err != nil {
		log(ctx).Debug("problem unmarshaling work", "err", err)
		return nil
	}

	for _, bookID := range bookIDs {
		log(ctx).Debug("ensuring work-edition edge", "workID", workID, "bookID", bookID)

		idx, found := slices.BinarySearchFunc(work.Books, bookID, func(b bookResource, id int64) int {
			return cmp.Compare(b.ForeignID, id)
		})

		workBytes, _, _, err = c.getter.GetBook(ctx, bookID)
		if err != nil {
			// Maybe the cache wasn't able to refresh because it was deleted? Move on.
			log(ctx).Warn("unable to ensure edition", "err", err, "workID", workID, "bookID", bookID)
			continue
		}

		var w workResource
		err = json.Unmarshal(workBytes, &w)
		if err != nil {
			log(ctx).Warn("problem unmarshaling work", "err", err)
			continue
		}

		// TODO: De-dupe on title?

		if found {
			work.Books[idx] = w.Books[0] // Replace.
		} else {
			work.Books = slices.Insert(work.Books, idx, w.Books[0]) // Insert.
		}
	}

	out, err := json.Marshal(work)
	if err != nil {
		return err
	}

	c.cache.Set(ctx, workKey(workID), out, 2*_workTTL)

	// We modified the work, so the author also needs to be updated. Remove the
	// relationship so it doesn't no-op during the ensure.
	go func() {
		c.ensureG.Go(func() error {
			ctx := context.Background()

			defer func() {
				if r := recover(); r != nil {
					log(ctx).Error("panic", "details", r)
				}
			}()

			for _, author := range work.Authors {
				c.ensureC <- edge{kind: authorEdge, parentID: author.ForeignID, childIDs: []int64{workID}}
			}

			return nil
		})
	}()

	return nil
}

// ensureWorks ensures that the given works exist on the author. This is a
// no-op if our cached work already includes the work's ID. This is meant to be
// invoked in the background, and it's what allows us to support large authors.
func (c *controller) ensureWorks(ctx context.Context, authorID int64, workIDs ...int64) error {
	if len(workIDs) == 0 {
		return nil
	}

	a, err := c.GetAuthor(ctx, authorID)
	if errors.Is(err, statusErr(http.StatusTooManyRequests)) {
		a, err = c.GetAuthor(ctx, authorID) // Reload if we got a cold cache.
	}
	if err != nil {
		log(ctx).Debug("problem loading author for ensureWorks", "err", err)
		return err
	}
	var author authorResource
	err = json.Unmarshal(a, &author)
	if err != nil {
		log(ctx).Debug("problem unmarshaling author", "err", err)
		return nil
	}

	for _, workID := range workIDs {
		log(ctx).Debug("ensuring author-work edge", "authorID", authorID, "workID", workID)

		idx, found := slices.BinarySearchFunc(author.Works, workID, func(w workResource, id int64) int {
			return cmp.Compare(w.ForeignID, id)
		})

		workBytes, _, err := c.getter.GetWork(ctx, workID)
		if err != nil {
			// Maybe the cache wasn't able to refresh because it was deleted? Move on.
			log(ctx).Warn("unable to ensure work", "err", err, "authorID", authorID, "workID", workID)
			continue
		}

		var work workResource
		err = json.Unmarshal(workBytes, &work)
		if err != nil {
			log(ctx).Warn("problem unmarshaling work", "err", err)
			continue
		}

		if len(work.Books) == 0 {
			log(ctx).Warn("work had no editions", "workID", workID)
			continue
		}

		if found {
			author.Works[idx] = work // Replace.
		} else {
			author.Works = slices.Insert(author.Works, idx, work) // Insert.
		}
	}

	author.Series = []seriesResource{}

	// Keep track of any duplicated titles so we can disambiguate them with subtitles.
	titles := map[string]int{}

	// Collect series and merge link items so each SeriesResource collects all
	// of the linked works.
	series := map[int64]*seriesResource{}
	ratingSum := int64(0)
	ratingCount := int64(0)
	for _, w := range author.Works {
		titles[w.Title]++
		for _, b := range w.Books {
			ratingCount += b.RatingCount
			ratingSum += b.RatingSum
		}
		for _, s := range w.Series {
			if ss, ok := series[s.ForeignID]; ok {
				ss.LinkItems = append(ss.LinkItems, s.LinkItems...)
				continue
			}
			series[s.ForeignID] = &s
		}
	}
	// Disambiguate works which share the same title by including subtitles.
	for idx := range author.Works {
		if titles[author.Works[idx].Title] <= 1 {
			continue
		}
		if author.Works[idx].FullTitle == "" {
			continue
		}
		author.Works[idx].Title = author.Works[idx].FullTitle
		for bidx := range author.Works[idx].Books {
			if author.Works[idx].Books[bidx].FullTitle == "" {
				continue
			}
			author.Works[idx].Books[bidx].Title = author.Works[idx].Books[bidx].FullTitle
		}
	}
	for _, s := range series {
		author.Series = append(author.Series, *s)
	}
	if ratingCount != 0 {
		author.AverageRating = float32(ratingSum) / float32(ratingCount)
	}

	out, err := json.Marshal(author)
	if err != nil {
		return err
	}

	c.cache.Set(ctx, authorKey(authorID), out, 2*_authorTTL)

	return nil
}
