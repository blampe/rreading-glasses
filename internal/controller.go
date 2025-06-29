package internal

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5/middleware"
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

	// _missingTTL is how long we'll wait before retrying a 404.
	_missingTTL = 7 * 24 * time.Hour

	// _unknownAuthor author corresponds to the "unknown" author which always
	// 404s. The valid "unknown" author ID seems to be 4699102 instead.
	_unknownAuthor = int64(22294257)
)

// Controller facilitates operations on our cache by scheduling background work
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
type Controller struct {
	cache  cache[[]byte]
	getter getter             // Core GetBook/GetAuthor/GetWork implementation.
	group  singleflight.Group // Coalesce lookups for the same key.

	// denormC erializes denormalization updates. This should only be used when
	// all resources have already been fetched.
	denormC       chan edge
	denormWaiting atomic.Int32 // How many denorm requests we have in the queue.

	// refreshG limits how many authors/works we sync in the background. Use
	// this to fetch things in the background in a bounded way.
	refreshG       errgroup.Group
	refreshWaiting atomic.Int32 // How many refresh requests we have in the queue.

	etagMatches    atomic.Int32 // How many times etags matched during denomalization.
	etagMismatches atomic.Int32 // How many times etags differed during denormalization.
}

// getter allows alternative implementations of the core logic to be injected.
// Don't write to the cache if you use it.
type getter interface {
	GetWork(ctx context.Context, workID int64, loadEditions editionsCallback) (_ []byte, authorID int64, _ error)
	GetBook(ctx context.Context, bookID int64, loadEditions editionsCallback) (_ []byte, workID int64, authorID int64, _ error) // Returns a serialized Work??
	GetAuthor(ctx context.Context, authorID int64) ([]byte, error)
	GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq[int64] // Returns book/edition IDs, not works.
}

// NewUpstream creates a new http.Client with middleware appropriate for use
// with an upstream.
func NewUpstream(host string, cookie string, proxy string) (*http.Client, error) {
	upstream := &http.Client{
		Transport: throttledTransport{
			// TODO: Unauthenticated defaults to 1 request per minute.
			Limiter: rate.NewLimiter(rate.Every(time.Hour/60), 1),
			RoundTripper: ScopedTransport{
				Host:         host,
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
			RoundTripper: ScopedTransport{
				Host: host,
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

// NewController creates a new controller. Background jobs to load author works
// and editions is bounded to at most 10 concurrent tasks.
func NewController(cache cache[[]byte], getter getter) (*Controller, error) {
	c := &Controller{
		cache:  cache,
		getter: getter,

		denormC: make(chan edge),
	}

	c.refreshG.SetLimit(10)

	// Log controller stats every minute.
	go func() {
		ctx := context.Background()
		for {
			time.Sleep(1 * time.Minute)
			etagHits, etagMisses := c.etagMatches.Load(), c.etagMismatches.Load()
			Log(ctx).Debug("controller stats",
				"refreshWaiting", c.refreshWaiting.Load(),
				"denormWaiting", c.denormWaiting.Load(),
				"etagMatches", etagHits,
				"etagRatio", float64(etagHits)/(float64(etagHits)+float64(etagMisses)),
			)
		}
	}()

	return c, nil
}

// GetBook loads a book (edition) or returns a cached value if one exists.
// TODO: This should only return a book!
func (c *Controller) GetBook(ctx context.Context, bookID int64) ([]byte, time.Duration, error) {
	p, err, _ := c.group.Do(BookKey(bookID), func() (any, error) {
		return c.getBook(ctx, bookID)
	})
	pair := p.(ttlpair)
	return pair.bytes, pair.ttl, err
}

// GetWork loads a work or returns a cached value if one exists.
func (c *Controller) GetWork(ctx context.Context, workID int64) ([]byte, time.Duration, error) {
	p, err, _ := c.group.Do(WorkKey(workID), func() (any, error) {
		return c.getWork(ctx, workID)
	})
	pair := p.(ttlpair)
	return pair.bytes, pair.ttl, err
}

// GetAuthor loads an author or returns a cached value if one exists.
func (c *Controller) GetAuthor(ctx context.Context, authorID int64) ([]byte, time.Duration, error) {
	// The "unknown author" ID is never loadable, so we can short-circuit.
	if authorID == _unknownAuthor {
		return nil, _authorTTL, errNotFound
	}
	p, err, _ := c.group.Do(AuthorKey(authorID), func() (any, error) {
		return c.getAuthor(ctx, authorID)
	})
	pair := p.(ttlpair)
	return pair.bytes, pair.ttl, err
}

func (c *Controller) getBook(ctx context.Context, bookID int64) (ttlpair, error) {
	workBytes, ttl, ok := c.cache.GetWithTTL(ctx, BookKey(bookID))
	if ok && ttl > 0 {
		if slices.Equal(workBytes, _missing) {
			return ttlpair{}, errNotFound
		}
		return ttlpair{bytes: workBytes, ttl: ttl}, nil
	}

	// Cache miss.
	workBytes, workID, _, err := c.getter.GetBook(ctx, bookID, c.loadEditions)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, BookKey(bookID), _missing, _missingTTL)
		return ttlpair{}, err
	}
	if err != nil {
		Log(ctx).Warn("problem getting book", "err", err, "bookID", bookID)
		return ttlpair{}, err
	}

	ttl = fuzz(_editionTTL, 2.0)
	c.cache.Set(ctx, BookKey(bookID), workBytes, ttl)

	if workID > 0 {
		// Ensure the edition/book is included with the work, but don't block the response.
		go func() {
			c.denormWaiting.Add(1)
			c.denormC <- edge{kind: workEdge, parentID: workID, childIDs: []int64{bookID}}
		}()
	}

	return ttlpair{bytes: workBytes, ttl: ttl}, nil
}

func (c *Controller) getWork(ctx context.Context, workID int64) (ttlpair, error) {
	cachedBytes, ttl, ok := c.cache.GetWithTTL(ctx, WorkKey(workID))
	if ok && ttl > 0 {
		if slices.Equal(cachedBytes, _missing) {
			return ttlpair{}, errNotFound
		}
		return ttlpair{bytes: cachedBytes, ttl: ttl}, nil
	}

	// Cache miss.
	workBytes, authorID, err := c.getter.GetWork(ctx, workID, c.loadEditions)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, WorkKey(workID), _missing, _missingTTL)
		return ttlpair{}, err
	}
	if err != nil {
		Log(ctx).Warn("problem getting work", "err", err, "workID", workID)
		return ttlpair{}, err
	}

	ttl = fuzz(_workTTL, 1.5)
	c.cache.Set(ctx, WorkKey(workID), workBytes, ttl)

	// Ensuring relationships doesn't block.
	go func() {
		c.refreshWaiting.Add(1)
		c.refreshG.Go(func() error {
			ctx := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("refresh-work-%d", workID))

			defer func() {
				c.refreshWaiting.Add(-1)
				if r := recover(); r != nil {
					Log(ctx).Error("panic", "details", r)
				}
			}()

			// Ensure we keep whatever editions we already had cached.
			var cached workResource
			_ = json.Unmarshal(cachedBytes, &cached)

			cachedBookIDs := []int64{}
			for _, b := range cached.Books {
				_, _, _ = c.GetBook(ctx, b.ForeignID) // Ensure fetched.
				cachedBookIDs = append(cachedBookIDs, b.ForeignID)
			}
			_, _, _ = c.GetAuthor(ctx, authorID) // Ensure fetched.

			// Free up the refresh group for someone else.
			go func() {
				c.denormWaiting.Add(int32(len(cachedBookIDs)))
				c.denormC <- edge{kind: workEdge, parentID: workID, childIDs: cachedBookIDs}

				if authorID > 0 {
					// Ensure the work belongs to its author.
					c.denormWaiting.Add(1)
					c.denormC <- edge{kind: authorEdge, parentID: authorID, childIDs: []int64{workID}}
				}
			}()

			return nil
		})
	}()

	// Return the last cached value to give the refresh time to complete.
	if len(cachedBytes) > 0 {
		return ttlpair{bytes: cachedBytes, ttl: ttl}, err
	}

	return ttlpair{bytes: workBytes, ttl: ttl}, err
}

func (c *Controller) loadEditions(grBookIDs ...int64) {
	go func() {
		c.refreshWaiting.Add(1)
		c.refreshG.Go(func() error {
			ctx := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("load-editions-%d", time.Now().Unix()))

			defer func() {
				c.refreshWaiting.Add(-1)
				if r := recover(); r != nil {
					Log(ctx).Error("panic", "details", r)
				}
			}()

			for _, grBookID := range grBookIDs {
				_, _, err := c.GetBook(ctx, grBookID)
				if err != nil {
					Log(ctx).Warn("problem loading edition", "grBookID", grBookID, "err", err)
				}
			}

			return nil
		})
	}()
}

// getAuthor returns an AuthorResource with up to 20 works populated. We
// persist data locally for 2x the TTL we give the client because stale data
// can be used to speed up subsequent cache misses / refreshes.
//
// NB: Author endpoints appear to have different rate limiting compared to
// works, YMMV.
func (c *Controller) getAuthor(ctx context.Context, authorID int64) (ttlpair, error) {
	cachedBytes, ttl, ok := c.cache.GetWithTTL(ctx, AuthorKey(authorID))
	if ok && ttl > 0 {
		if slices.Equal(cachedBytes, _missing) {
			return ttlpair{}, errNotFound
		}
		return ttlpair{bytes: cachedBytes, ttl: ttl}, nil
	}

	// Cache miss.
	authorBytes, err := c.getter.GetAuthor(ctx, authorID)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, AuthorKey(authorID), _missing, _missingTTL)
		return ttlpair{}, err
	}
	if err != nil {
		Log(ctx).Warn("problem getting author", "err", err, "authorID", authorID)
		return ttlpair{}, err
	}

	ttl = fuzz(_authorTTL, 1.5)
	c.cache.Set(ctx, AuthorKey(authorID), authorBytes, ttl)

	// Ensuring relationships doesn't block.
	go func() {
		c.refreshWaiting.Add(1)
		c.refreshG.Go(func() error {
			ctx := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("refresh-author-%d", authorID))

			defer func() {
				c.refreshWaiting.Add(-1)
				if r := recover(); r != nil {
					Log(ctx).Error("panic", "details", r)
				}
			}()

			// Ensure we keep whatever works we already had cached.
			var cached AuthorResource
			_ = json.Unmarshal(cachedBytes, &cached)

			workIDSToDenormalize := []int64{}
			for _, w := range cached.Works {
				_, _, _ = c.GetWork(ctx, w.ForeignID) // Ensure fetched before denormalizing.
				workIDSToDenormalize = append(workIDSToDenormalize, w.ForeignID)
			}

			// Finally try to load all of the author's works to ensure we have them.
			n := 0
			start := time.Now()
			Log(ctx).Info("fetching all works for author", "authorID", authorID)
			for bookID := range c.getter.GetAuthorBooks(ctx, authorID) {
				if n > 1000 {
					break
				}
				bookBytes, _, err := c.GetBook(ctx, bookID)
				if err != nil {
					Log(ctx).Warn("problem getting book for author", "authorID", authorID, "bookID", bookID, "err", err)
					continue
				}
				var w workResource
				_ = json.Unmarshal(bookBytes, &w)
				workID := w.ForeignID
				_, _, _ = c.GetWork(ctx, workID) // Ensure fetched before denormalizing.
				workIDSToDenormalize = append(workIDSToDenormalize, workID)
				n++
			}

			slices.Sort(workIDSToDenormalize)
			workIDSToDenormalize = slices.Compact(workIDSToDenormalize)

			if len(workIDSToDenormalize) > 0 {
				// Don't block so we can free up the refresh group for someone else.
				go func() {
					c.denormWaiting.Add(int32(len(workIDSToDenormalize)))
					c.denormC <- edge{kind: authorEdge, parentID: authorID, childIDs: workIDSToDenormalize}
				}()
			}
			Log(ctx).Info("fetched all works for author", "authorID", authorID, "count", len(workIDSToDenormalize), "duration", time.Since(start).String())

			return nil
		})
	}()

	// Return the last cached value to give the refresh time to complete.
	if len(cachedBytes) > 0 {
		return ttlpair{bytes: cachedBytes, ttl: ttl}, err
	}

	return ttlpair{bytes: authorBytes, ttl: ttl}, nil
}

// Run is responsible for denormalizing data. Race conditions are still
// possible but less likely by serializing updates this way.
func (c *Controller) Run(ctx context.Context, wait time.Duration) {
	for edge := range groupEdges(ctx, c.denormC, wait) {
		c.denormWaiting.Add(-int32(len(edge.childIDs)))

		ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
		ctx = context.WithValue(ctx, middleware.RequestIDKey, fmt.Sprintf("denorm-%d-%d", edge.kind, edge.parentID))

		switch edge.kind {
		case authorEdge:
			if edge.parentID == _unknownAuthor {
				break
			}
			if err := c.denormalizeWorks(ctx, edge.parentID, edge.childIDs...); err != nil {
				Log(ctx).Warn("problem ensuring work", "err", err, "authorID", edge.parentID, "workIDs", edge.childIDs)
			}
		case workEdge:
			if err := c.denormalizeEditions(ctx, edge.parentID, edge.childIDs...); err != nil {
				Log(ctx).Warn("problem ensuring edition", "err", err, "workID", edge.parentID, "bookIDs", edge.childIDs)
			}
		}
		cancel()
	}
}

// Shutdown waits for all refresh and denormalization goroutines to finish
// submitting their work and then closes the denormalization channel. Run will
// run to completion after Shutdown is called.
func (c *Controller) Shutdown(ctx context.Context) {
	// _ = c.refreshG.Wait()
	//
	//	for c.denormWaiting.Load() > 0 {
	//		time.Sleep(1 * time.Second)
	//	}
}

// denormalizeEditions ensures that the given editions exists on the work. It
// deserializes the target work once. (TODO: No-op if it includes an edition
// with the same language and title).
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
func (c *Controller) denormalizeEditions(ctx context.Context, workID int64, bookIDs ...int64) error {
	if len(bookIDs) == 0 {
		return nil
	}

	workBytes, _, err := c.getter.GetWork(ctx, workID, nil)
	if err != nil {
		Log(ctx).Debug("problem getting work", "err", err)
		return err
	}

	old := newETagWriter()
	r := io.TeeReader(bytes.NewReader(workBytes), old)

	var work workResource
	err = json.NewDecoder(r).Decode(&work)
	if err != nil {
		Log(ctx).Debug("problem unmarshaling work", "err", err, "workID", workID)
		_ = c.cache.Expire(ctx, WorkKey(workID))
		return err
	}

	Log(ctx).Debug("ensuring work-edition edges", "workID", workID, "bookIDs", bookIDs)

	for _, bookID := range bookIDs {

		idx, found := slices.BinarySearchFunc(work.Books, bookID, func(b bookResource, id int64) int {
			return cmp.Compare(b.ForeignID, id)
		})

		// TODO: Pre-fetch these in parallel.
		workBytes, _, _, err = c.getter.GetBook(ctx, bookID, nil)
		if err != nil {
			// Maybe the cache wasn't able to refresh because it was deleted? Move on.
			Log(ctx).Warn("unable to denormalize edition", "err", err, "workID", workID, "bookID", bookID)
			continue
		}

		var w workResource
		err = json.Unmarshal(workBytes, &w)
		if err != nil {
			Log(ctx).Warn("problem unmarshaling book", "err", err, "bookID", bookID)
			_ = c.cache.Expire(ctx, BookKey(bookID))
			continue
		}

		// TODO: De-dupe on title?

		if found {
			work.Books[idx] = w.Books[0] // Replace.
		} else {
			work.Books = slices.Insert(work.Books, idx, w.Books[0]) // Insert.
		}
	}

	// Sanity check that our invariant holds. There should not be any dupes.
	slices.SortFunc(work.Books, func(l, r bookResource) int {
		return cmp.Compare(l.ForeignID, r.ForeignID)
	})
	compacted := slices.CompactFunc(work.Books, func(l, r bookResource) bool {
		return l.ForeignID == r.ForeignID
	})
	if len(compacted) != len(work.Books) {
		Log(ctx).Warn("broken work invariant", "workID", workID, "compacted", len(compacted), "original", len(work.Books))
		work.Books = compacted
	}

	buf := _buffers.Get()
	defer buf.Free()
	neww := newETagWriter()
	w := io.MultiWriter(buf, neww)
	err = json.NewEncoder(w).Encode(work)
	if err != nil {
		return err
	}

	if neww.ETag() == old.ETag() {
		// The work didn't change, so we're done.
		c.etagMatches.Add(1)
		return nil
	}
	c.etagMismatches.Add(1)

	// We can't persist the shared buffer in the cache so clone it.
	out := bytes.Clone(buf.Bytes())

	c.cache.Set(ctx, WorkKey(workID), out, fuzz(_workTTL, 1.5))

	// We modified the work, so the author also needs to be updated. Remove the
	// relationship so it doesn't no-op during the denormalization.
	go func() {
		for _, author := range work.Authors {
			c.denormWaiting.Add(1)
			c.denormC <- edge{kind: authorEdge, parentID: author.ForeignID, childIDs: []int64{workID}}
		}
	}()

	return nil
}

// denormalizeWorks ensures that the given works exist on the author. This is a
// no-op if our cached work already includes the work's ID. This is meant to be
// invoked in the background, and it's what allows us to support large authors.
func (c *Controller) denormalizeWorks(ctx context.Context, authorID int64, workIDs ...int64) error {
	if len(workIDs) == 0 {
		return nil
	}

	authorBytes, _, err := c.GetAuthor(ctx, authorID)
	if errors.Is(err, statusErr(http.StatusTooManyRequests)) {
		authorBytes, _, err = c.GetAuthor(ctx, authorID) // Reload if we got a cold cache.
	}
	if err != nil {
		Log(ctx).Debug("problem loading author for denormalizeWorks", "err", err)
		return err
	}

	old := newETagWriter()
	r := io.TeeReader(bytes.NewReader(authorBytes), old)

	var author AuthorResource
	err = json.NewDecoder(r).Decode(&author)
	if err != nil {
		Log(ctx).Debug("problem unmarshaling author", "err", err, "authorID", authorID)
		_ = c.cache.Expire(ctx, AuthorKey(authorID))
		return err
	}

	Log(ctx).Debug("ensuring author-work edges", "authorID", authorID, "workIDs", workIDs)

	for _, workID := range workIDs {

		idx, found := slices.BinarySearchFunc(author.Works, workID, func(w workResource, id int64) int {
			return cmp.Compare(w.ForeignID, id)
		})

		workBytes, _, err := c.getter.GetWork(ctx, workID, nil)
		if err != nil {
			// Maybe the cache wasn't able to refresh because it was deleted? Move on.
			Log(ctx).Warn("unable to denormalize work", "err", err, "authorID", authorID, "workID", workID)
			continue
		}

		var work workResource
		err = json.Unmarshal(workBytes, &work)
		if err != nil {
			Log(ctx).Warn("problem unmarshaling work", "err", err, "workID", workID)
			_ = c.cache.Expire(ctx, WorkKey(workID))
			continue
		}

		if len(work.Books) == 0 {
			Log(ctx).Warn("work had no editions", "workID", workID)
			continue
		}

		if found {
			author.Works[idx] = work // Replace.
		} else {
			author.Works = slices.Insert(author.Works, idx, work) // Insert.
		}
	}

	// Sanity check that our invariant holds. There should not be any dupes.
	slices.SortFunc(author.Works, func(l, r workResource) int {
		return cmp.Compare(l.ForeignID, r.ForeignID)
	})
	compacted := slices.CompactFunc(author.Works, func(l, r workResource) bool {
		return l.ForeignID == r.ForeignID
	})
	if len(compacted) != len(author.Works) {
		Log(ctx).Warn("broken author invariant", "authorID", authorID, "compacted", len(compacted), "original", len(author.Works))
		author.Works = compacted
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
		if w.ShortTitle != "" {
			titles[strings.ToUpper(w.ShortTitle)]++
		} else {
			titles[strings.ToUpper(w.Title)]++
		}
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
		shortTitle := author.Works[idx].Title
		if author.Works[idx].ShortTitle != "" {
			shortTitle = author.Works[idx].ShortTitle
		}
		// If this is part of a series, always include the subtitle.
		inSeries := len(author.Works[idx].Series) > 0
		if !inSeries && titles[strings.ToUpper(shortTitle)] <= 1 {
			// If the short title is already unique there's nothing to do.
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

	buf := _buffers.Get()
	defer buf.Free()
	neww := newETagWriter()
	w := io.MultiWriter(buf, neww)
	err = json.NewEncoder(w).Encode(author)
	if err != nil {
		return err
	}

	if neww.ETag() == old.ETag() {
		// The author didn't change, so we're done.
		c.etagMatches.Add(1)
		return nil
	}
	c.etagMismatches.Add(1)

	// We can't persist the shared buffer in the cache so clone it.
	out := bytes.Clone(buf.Bytes())

	c.cache.Set(ctx, AuthorKey(authorID), out, fuzz(_authorTTL, 1.5))

	return nil
}

// editionsCallback can be used by a Getter to trigger async loading of
// additional editions.
type editionsCallback func(grBookIDs ...int64)

func fuzz(d time.Duration, f float64) time.Duration {
	if f > 1.0 {
		f = 1.0
	}
	factor := 1.0 + rand.Float64()*(f-1.0)
	return time.Duration(float64(d) * factor)
}

type ttlpair struct {
	bytes []byte
	ttl   time.Duration
}
