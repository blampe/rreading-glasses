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
	"sync"
	"time"

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

	authors edges          // Tracks Author->Works.
	works   edges          // Tracks Work->Editions.
	ensureC chan edge      // Serializes edge updates.
	ensureG sync.WaitGroup // Tracks ensure goroutines.

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

		authors: edges{},
		works:   edges{},
		ensureC: make(chan edge),
	}

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
		// Ensure the edition/book is included with the work, but don't block.
		c.ensureG.Add(1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log(ctx).Error("panic", "details", r)
				}
			}()
			defer c.ensureG.Done()

			c.ensureC <- edge{kind: workEdge, parentID: workID, childID: bookID}
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
	c.ensureG.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log(ctx).Error("panic", "details", r)
			}
		}()
		defer c.ensureG.Done()

		// Ensure we keep whatever editions we already had cached.
		var cached workResource
		_ = json.Unmarshal(cachedBytes, &cached)
		for _, b := range cached.Books {
			c.ensureC <- edge{kind: workEdge, parentID: workID, childID: b.ForeignID}
		}

		if authorID > 0 {
			// Ensure the work belongs to its author.
			c.ensureC <- edge{kind: authorEdge, parentID: authorID, childID: workID}
		}
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
	c.ensureG.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log(ctx).Error("panic", "details", r)
			}
		}()
		defer c.ensureG.Done()

		// Ensure we keep whatever works we already had cached.
		var cached authorResource
		_ = json.Unmarshal(cachedBytes, &cached)
		for _, w := range cached.Works {
			c.ensureC <- edge{kind: authorEdge, parentID: authorID, childID: w.ForeignID}
		}

		// Finally try to load all of the author's works to ensure we have them.
		for bookID := range c.getter.GetAuthorBooks(context.Background(), authorID) {
			// TODO: book edge
			_, _ = c.GetBook(context.Background(), bookID)
		}
	}()

	// If we didn't have anything cached then this is the first time loading
	// the author. Return a 429 so the client will retry in 5 seconds. That
	// will give us time to load some works on the author.
	if !ok {
		return nil, statusErr(http.StatusTooManyRequests)
	}

	return authorBytes, nil
}

// Run is responsible for denormalizing data. Race conditions are still
// possible but less likely by serializing updates this way.
func (c *controller) Run(ctx context.Context) {
	for edge := range c.ensureC {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		switch edge.kind {
		case authorEdge:
			if err := c.ensureWork(ctx, edge.parentID, edge.childID); err != nil {
				log(ctx).Warn("problem ensuring work", "err", err, "authorID", edge.parentID, "workID", edge.childID)
			}
		case workEdge:
			if err := c.ensureEdition(ctx, edge.parentID, edge.childID); err != nil {
				log(ctx).Warn("problem ensuring edition", "err", err, "workID", edge.parentID, "bookID", edge.childID)
			}
		}
		cancel()
	}
}

// Shutdown waits for all "ensure" goroutines to finish submitting their work
// and then closes the ensure channel. Run will run to completion after
// Shutdown is called.
func (c *controller) Shutdown(ctx context.Context) {
	c.ensureG.Wait()
	close(c.ensureC)
}

// ensureEdition ensures that the given edition exists on the work. This is a
// no-op if our cached work already includes the edition's ID (TODO: OR if it
// includes an edition with the same title).
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
func (c *controller) ensureEdition(ctx context.Context, workID int64, bookID int64) error {
	if c.works.Contains(workID, bookID) {
		return nil
	}

	log(ctx).Debug("ensuring work-edition edge", "workID", workID, "bookID", bookID)

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

	// TODO: This is needed because we didn't always keep these sorted. Remove
	// it after folks have had enough time to upgrade and cache entries have
	// expired.
	slices.SortFunc(work.Books, func(left, right bookResource) int {
		return cmp.Compare(left.ForeignID, right.ForeignID)
	})

	idx, found := slices.BinarySearchFunc(work.Books, bookID, func(b bookResource, id int64) int {
		return cmp.Compare(b.ForeignID, id)
	})

	workBytes, _, _, err = c.getter.GetBook(ctx, bookID)
	if err != nil {
		// Maybe the cache wasn't able to refresh because it was deleted? Move on.
		log(ctx).Warn("unable to ensure edition", "err", err, "workID", workID, "bookID", bookID)
		return err
	}

	var w workResource
	err = json.Unmarshal(workBytes, &w)
	if err != nil {
		log(ctx).Warn("problem unmarshaling work", "err", err)
		return err
	}

	// TODO: De-dupe on title?

	if found {
		work.Books[idx] = w.Books[0] // Replace.
	} else {
		work.Books = slices.Insert(work.Books, idx, w.Books[0]) // Insert.
	}

	out, err := json.Marshal(work)
	if err != nil {
		return err
	}

	c.cache.Set(ctx, workKey(workID), out, 2*_workTTL)

	// Record the relationship so we don't re-add this.
	c.works.Add(workID, bookID)

	// We modified the work, so the author also needs to be updated. Remove the
	// relationship so it doesn't no-op during the ensure.
	c.ensureG.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log(ctx).Error("panic", "details", r)
			}
		}()
		defer c.ensureG.Done()

		for _, author := range work.Authors {
			c.authors.Remove(author.ForeignID, workID)
			c.ensureC <- edge{kind: authorEdge, parentID: author.ForeignID, childID: workID}
		}
	}()

	return nil
}

// ensureWork ensures that the given works exist on the author. This is a
// no-op if our cached work already includes the work's ID. This is meant to be
// invoked in the background, and it's what allows us to support large authors.
func (c *controller) ensureWork(ctx context.Context, authorID int64, workID int64) error {
	if c.authors.Contains(authorID, workID) {
		return nil
	}

	log(ctx).Debug("ensuring author-work edge", "authorID", authorID, "workID", workID)

	a, err := c.GetAuthor(ctx, authorID)
	if errors.Is(err, statusErr(http.StatusTooManyRequests)) {
		a, err = c.GetAuthor(ctx, authorID) // Reload if we got a cold cache.
	}
	if err != nil {
		return err
	}
	var author authorResource
	err = json.Unmarshal(a, &author)
	if err != nil {
		return nil
	}

	// TODO: This is needed because we didn't always keep these sorted. Remove
	// it after folks have had enough time to upgrade and cache entries have
	// expired.
	slices.SortFunc(author.Works, func(left, right workResource) int {
		return cmp.Compare(left.ForeignID, right.ForeignID)
	})

	idx, found := slices.BinarySearchFunc(author.Works, workID, func(w workResource, id int64) int {
		return cmp.Compare(w.ForeignID, id)
	})

	workBytes, _, err := c.getter.GetWork(ctx, workID)
	if err != nil {
		// Maybe the cache wasn't able to refresh because it was deleted? Move on.
		log(ctx).Warn("unable to ensure work", "err", err, "authorID", authorID, "workID", workID)
		return nil
	}

	var work workResource
	err = json.Unmarshal(workBytes, &work)
	if err != nil {
		log(ctx).Warn("problem unmarshaling work", "err", err)
		return err
	}

	if len(work.Books) == 0 {
		log(ctx).Warn("work had no editions", "workID", workID)
		return nil
	}

	if found {
		author.Works[idx] = work // Replace.
	} else {
		author.Works = slices.Insert(author.Works, idx, work) // Insert.
	}

	author.Series = []seriesResource{}

	// Collect series and merge link items so each SeriesResource collects all
	// of the linked works.
	// TODO: Do this in the controller.
	series := map[int64]*seriesResource{}
	ratingSum := int64(0)
	ratingCount := int64(0)
	for _, w := range author.Works {
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

	// Record the relationship so we don't re-add this.
	c.authors.Add(authorID, workID)

	return nil
}
