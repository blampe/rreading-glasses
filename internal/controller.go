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
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/option"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
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

	_seriesTTL = 14 * 24 * time.Hour // 2 weeks

	// _missing is a sentinel value we cache for 404 responses.
	_missing = []byte{0}

	// _missingTTL is how long we'll wait before retrying a 404.
	_missingTTL = 7 * 24 * time.Hour
)

// unknownAuthor author corresponds to the "unknown" or "anonymous" authors
// which always 404. The valid "unknown" author ID seems to be 4699102 instead.
func unknownAuthor(authorID int64) bool {
	return authorID == 22294257 || authorID == 5158478 || authorID == 5481957 || authorID == 4699102 ||
		authorID == 14144674 || // SuperSummary, 10k works
		authorID == 5153555 || // Wikipedia, 120k
		authorID == 4340042 // Books LLC, 31k
}

// Controller facilitates operations on our cache by scheduling background work
// and handling cache invalidation.
//
// Most operations take place inside a singleflight group to prevent redundant
// work.
//
// The request path is limited to Get methods which at worst perform only O(1)
// lookups. More expensive work, like denormalization, is handled in the
// background. The original metadata server likely does this work in the
// request path, hence why larger authors don't work -- it can't complete
// O(work * editions) within the lifespan of the request.
//
// Another significant difference is that we cache data eagerly, when it is
// requested. We don't require a full database dump, so we're able to grab new
// works as soon as they're available.
type Controller struct {
	cache     cache[[]byte]
	getter    getter             // Core GetBook/GetAuthor/GetWork implementation.
	persister persister          // persister tracks state across reboots.
	group     singleflight.Group // Coalesce lookups for the same key.

	// denormC erializes denormalization updates. This should only be used when
	// all resources have already been fetched.
	denormC chan edge
	grouper grouper

	// refreshG limits how many authors/works we sync in the background. Use
	// this to fetch things in the background in a bounded way.
	refreshG errgroup.Group

	metrics *controllerMetrics
}

// getter allows alternative implementations of the core logic to be injected.
// Don't write to the cache if you use it.
type getter interface {
	// GetWork gets the work with the given ID. A work is an abstract
	// collection of editions. The saveEditions callback can be invoked if the
	// work can be loaded with editions in one request, to reduce load
	// upstream. When authorID is returned the work will be denormalized to the
	// author.
	//
	// A serialized workResource is returned.
	GetWork(ctx context.Context, workID int64, saveEditions editionsCallback) (_ []byte, authorID int64, _ error)

	// GetBook gets an individual edition of a work. The saveEditions
	// callback can be invoked if the edition can be loaded with other editions
	// in one request, to reduce load upstream.
	//
	// Confusingly, a serialized workResource is also returned here. It should
	// be a valid work and it should include one book (the one being loaded).
	GetBook(ctx context.Context, bookID int64, saveEditions editionsCallback) (_ []byte, workID int64, authorID int64, _ error) // Returns a serialized Work??

	// GetAuthor gets an author's details.
	//
	// A serialized AuthorResource is returned.
	GetAuthor(ctx context.Context, authorID int64) ([]byte, error)

	GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq[int64] // Returns book/edition IDs, not works.

	// GetSeries returns a list of works contained in a series. The works may
	// not all be by the same author.
	GetSeries(ctx context.Context, seriesID int64) (*SeriesResource, error)

	// Search performs a natural language query against the upstream (or other
	// search index).
	//
	// A serialied searchResource is returned.
	Search(ctx context.Context, query string) ([]SearchResource, error)

	// Recommendations returns a list of work IDs which are trending or popular.
	// Eventually we may consider implementing OAuth in order to return
	// custom-tailored recommendations.
	Recommendations(ctx context.Context, page int64) (RecommentationsResource, error)
}

// NewUpstream creates a new http.Client with middleware appropriate for use
// with an upstream.
func NewUpstream(host string, proxy string) (*http.Client, error) {
	upstream := &http.Client{
		Transport: throttledTransport{
			Limiter: rate.NewLimiter(rate.Every(time.Second/3), 1),
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
func NewController(cache cache[[]byte], getter getter, persister persister, reg *prometheus.Registry) (*Controller, error) {
	metrics := newControllerMetrics(reg)
	c := &Controller{
		cache:     cache,
		getter:    getter,
		persister: &nopersist{},
		metrics:   metrics,
		grouper:   grouper{metrics: metrics},

		denormC: make(chan edge),
	}
	if persister != nil {
		c.persister = persister
	}

	c.refreshG.SetLimit(15)

	// Log controller stats every minute.
	go func() {
		ctx := context.Background()
		for {
			time.Sleep(1 * time.Minute)
			Log(ctx).Debug("controller stats",
				"refreshWaiting", c.metrics.refreshWaitingGet(),
				"denormWaiting", c.metrics.denormWaitingGet(),
				"etagMatches", c.metrics.etagMatchesGet(),
				"etagRatio", c.metrics.etagRatioGet(),
			)
		}
	}()

	// Retry any author refreshes that were in-flight when we last shut down.
	go func() {
		ctx := context.WithValue(context.Background(), middleware.RequestIDKey, "recovery")
		authorIDs, err := c.persister.Persisted(ctx)
		if err != nil {
			Log(ctx).Error("problem retrying in-flight refreshes", "err", err)
		}
		for _, authorID := range authorIDs {
			Log(ctx).Debug("resuming author refresh", "authorID", authorID)
			c.refreshAuthor(ctx, authorID, nil)
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

// Search queries the metadata provider.
func (c *Controller) Search(ctx context.Context, query string) ([]SearchResource, error) {
	if _asin.Match([]byte(query)) {
		// Try an ASIN lookup and fall back to regular search if that doesn't work.
		if results := c.searchASIN(ctx, query); len(results) > 0 {
			return results, nil
		}
	}
	return c.getter.Search(ctx, query)
}

// Recommendations returns recommended work IDs.
func (c *Controller) Recommendations(ctx context.Context, page int64) (RecommentationsResource, error) {
	recs, err := c.getter.Recommendations(ctx, page)
	if err != nil {
		return recs, err
	}

	// Try to fetch everything and return only the stuff that won't 404.
	mu := sync.Mutex{}
	wg := sync.WaitGroup{}
	workIDs := []int64{}

	for _, workID := range recs.WorkIDs {
		wg.Add(1)
		go func() {
			_, _, err := c.GetWork(ctx, workID)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			workIDs = append(workIDs, workID)
		}()
	}
	recs.WorkIDs = workIDs
	return recs, nil
}

func (c *Controller) searchASIN(ctx context.Context, asin string) []SearchResource {
	editionID, err := c.GetASIN(ctx, asin)
	if err != nil {
		return nil
	}

	workBytes, _, err := c.GetBook(ctx, editionID)
	if err != nil {
		return nil
	}

	var workRsc workResource
	err = json.Unmarshal(workBytes, &workRsc)
	if err != nil {
		return nil
	}

	return []SearchResource{{
		BookID: workRsc.Books[0].ForeignID,
		WorkID: workRsc.ForeignID,
		Author: SearchResourceAuthor{
			ID: workRsc.Authors[0].ForeignID,
		},
	}}
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
	if unknownAuthor(authorID) {
		return nil, _missingTTL, errNotFound
	}
	p, err, _ := c.group.Do(AuthorKey(authorID), func() (any, error) {
		return c.getAuthor(ctx, authorID)
	})
	pair := p.(ttlpair)
	return pair.bytes, pair.ttl, err
}

// GetSeries returns a cached series if one exists.
func (c *Controller) GetSeries(ctx context.Context, seriesID int64) ([]byte, error) {
	out, err, _ := c.group.Do(seriesKey(seriesID), func() (any, error) {
		return c.getSeries(ctx, seriesID)
	})
	return out.([]byte), err
}

// GetASIN returns the best known edition ID for the given ASIN, or a not found
// error if there is none.
func (c *Controller) GetASIN(ctx context.Context, asin string) (int64, error) {
	out, err, _ := c.group.Do(asin, func() (any, error) {
		return c.getASIN(ctx, asin)
	})
	return out.(int64), err
}

func (c *Controller) getASIN(ctx context.Context, asin string) (int64, error) {
	bytes, ok := c.cache.Get(ctx, asinKey(asin))
	if !ok {
		return 0, errNotFound
	}

	var asinRsc asinResource
	err := json.Unmarshal(bytes, &asinRsc)
	if err != nil {
		return 0, fmt.Errorf("unmarshaling for asin: %w", err)
	}

	return asinRsc.EditionID, nil
}

func (c *Controller) setASIN(ctx context.Context, asin string, editionID int64) error {
	bytes, err := json.Marshal(asinResource{EditionID: editionID})
	if err != nil {
		return fmt.Errorf("marshaling for asin: %w", err)
	}
	c.cache.Set(ctx, asinKey(asin), bytes, 24*time.Hour*365)
	return nil
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
	workBytes, workID, authorID, err := c.getter.GetBook(ctx, bookID, c.saveEditions)
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
			if _, _, err := c.GetWork(ctx, workID); err != nil { // Ensure fetched.
				Log(ctx).Warn("skipping work denorm due to error", "bookID", bookID, "workID", workID, "err", err)
				return
			}
			if _, _, err := c.GetAuthor(ctx, authorID); err != nil { // Ensure fetched.
				Log(ctx).Warn("skipping work denorm due to error", "bookID", bookID, "authorID", authorID, "err", err)
				return
			}
			c.add(edge{kind: workEdge, parentID: workID, childIDs: newSet(bookID)})
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
	workBytes, authorID, err := c.getter.GetWork(ctx, workID, c.saveEditions)
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
	// todo: refreshWork similar to refreshAuthor
	go func() {
		c.metrics.refreshWaitingAdd(1)
		c.refreshG.Go(func() error {
			ctx := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("refresh-work-%d", workID))

			defer func() {
				c.metrics.refreshWaitingAdd(-1)
				if r := recover(); r != nil {
					Log(ctx).Error("panic", "details", r)
				}
			}()

			// Ensure we keep whatever editions we already had cached.
			var cached workResource
			_ = json.Unmarshal(cachedBytes, &cached)

			cachedBookIDs := []int64{}
			for _, b := range cached.Books {
				if _, _, err := c.GetBook(ctx, b.ForeignID); err == nil { // Ensure fetched.
					cachedBookIDs = append(cachedBookIDs, b.ForeignID)
				}
			}

			if authorID > 0 {
				_, _, _ = c.GetAuthor(ctx, authorID) // Ensure fetched.
			}

			// Free up the refresh group for someone else.
			go func() {
				c.add(edge{kind: workEdge, parentID: workID, childIDs: newSet(cachedBookIDs...)})

				if authorID > 0 {
					// Ensure the work belongs to its author.
					c.add(edge{kind: authorEdge, parentID: authorID, childIDs: newSet(workID)})
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

func (c *Controller) getSeries(ctx context.Context, seriesID int64) ([]byte, error) {
	seriesBytes, ttl, ok := c.cache.GetWithTTL(ctx, seriesKey(seriesID))
	if ok && ttl > 0 {
		if slices.Equal(seriesBytes, _missing) {
			return nil, errNotFound
		}
		return seriesBytes, nil
	}

	Log(ctx).Debug("getting series", "seriesID", seriesID)

	series, err := c.getter.GetSeries(ctx, seriesID)
	if err != nil {
		Log(ctx).Warn("problem getting series", "seriesID", seriesID, "err", err)
		return nil, err
	}

	out, err := json.Marshal(series)
	if err != nil {
		return nil, err
	}

	c.cache.Set(ctx, seriesKey(seriesID), out, _seriesTTL)

	return out, nil
}

func (c *Controller) saveEditions(grBooks ...workResource) {
	go func() {
		ctx := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("save-editions-%d", time.Now().Unix()))

		var grWorkID int64
		grBookIDs := []int64{}

		for _, w := range grBooks {
			if len(w.Books) != 1 {
				// We expect a single book wrapped in a work -- side effect of R's odd data model.
				Log(ctx).Warn("malformed edition", "grWorkID", w.ForeignID)
				continue
			}
			if grWorkID == 0 {
				grWorkID = w.ForeignID
			}
			if w.ForeignID != grWorkID {
				// Editions should all belong to the same work.
				Log(ctx).Warn("work-edition mismatch", "expected", grWorkID, "got", w.ForeignID)
				continue
			}
			if len(w.Authors) == 0 {
				Log(ctx).Warn("missing author", "workID", w.ForeignID)
				continue
			}
			authorID := w.Authors[0].ForeignID
			if _, _, err := c.GetAuthor(ctx, authorID); err != nil { // Ensure fetched.
				continue
			}

			if len(w.Books) == 0 {
				Log(ctx).Warn("missing books", "workID", w.ForeignID)
				continue
			}
			book := w.Books[0]

			if book.Asin != "" && _asin.Match([]byte(book.Asin)) {
				Log(ctx).Debug("found asin", "editionID", book.ForeignID, "asin", book.Asin)
				if err := c.setASIN(ctx, book.Asin, book.ForeignID); err != nil {
					Log(ctx).Warn("problem persisting asin", "editionID", book.ForeignID, "asin", book.Asin)
				}
			}

			if len(book.Contributors) == 0 {
				Log(ctx).Warn("missing contributors", "workID", w.ForeignID, "editionID", book.ForeignID)
				continue
			}
			if book.Contributors[0].ForeignID != authorID {
				continue // Skip editions not attributed to this author.
			}

			out, err := json.Marshal(w)
			if err != nil {
				continue
			}
			c.cache.Set(ctx, BookKey(book.ForeignID), out, fuzz(_editionTTL, 2.0))
			grBookIDs = append(grBookIDs, book.ForeignID)
		}

		if grWorkID == 0 || len(grBookIDs) == 0 {
			return // Shouldn't happen.
		}

		c.add(edge{kind: workEdge, parentID: grWorkID, childIDs: newSet(grBookIDs...)})
	}()
}

// getAuthor returns an AuthorResource with up to 20 works populated on first
// load. Additional works are populated asynchronously. The previous state is
// returned while a refresh is ongoing.
func (c *Controller) getAuthor(ctx context.Context, authorID int64) (ttlpair, error) {
	// We prefer a refresh key, if one exists, because it contains the author's
	// state prior to refreshing.
	preRefreshBytes, ok := c.cache.Get(ctx, refreshAuthorKey(authorID))
	if ok {
		if slices.Equal(preRefreshBytes, _missing) {
			return ttlpair{}, errNotFound
		}
		return ttlpair{bytes: preRefreshBytes, ttl: time.Hour}, nil
	}

	// If we're not refreshing then return the cached value as long as it's
	// still valid.
	cachedBytes, ttl, ok := c.cache.GetWithTTL(ctx, AuthorKey(authorID))
	if ok && ttl > 0 {
		if slices.Equal(cachedBytes, _missing) {
			return ttlpair{}, errNotFound
		}
		return ttlpair{bytes: cachedBytes, ttl: ttl}, nil
	}

	// Cache miss. Fetch new data.
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

	// From here we'll prefer to use the last-known state. If this is the first
	// time we've loaded the author we won't have previous state, so use
	// whatever we just fetched.
	if len(cachedBytes) == 0 {
		cachedBytes = authorBytes
	}

	// Mark the author as being refreshed by recording its last known state.
	if err := c.persister.Persist(ctx, authorID, cachedBytes); err != nil {
		Log(ctx).Warn("problem persisting refresh", "err", err)
	}

	// Kick off a refresh but don't block on it.
	go c.refreshAuthor(context.Background(), authorID, cachedBytes)

	// Return the last cached value to give the refresh time to complete.
	return ttlpair{bytes: cachedBytes, ttl: ttl}, nil
}

func (c *Controller) refreshAuthor(ctx context.Context, authorID int64, cachedBytes []byte) {
	c.metrics.refreshWaitingAdd(1)
	c.refreshG.Go(func() error {
		ctx = context.WithValue(ctx, middleware.RequestIDKey, fmt.Sprintf("refresh-author-%d", authorID))

		defer func() {
			if r := recover(); r != nil {
				Log(ctx).Error("panic", "details", r)
			}
		}()

		Log(ctx).Info("fetching all works for author", "authorID", authorID)

		n := 0
		start := time.Now()
		workIDSToDenormalize := []int64{}

		for bookID := range c.getter.GetAuthorBooks(ctx, authorID) {
			if n > 1000 {
				Log(ctx).Warn("found too many editions", "authorID", authorID)
				break // Some authors (e.g. Wikipedia) have an obscene number of works. Give up.
			}
			bookBytes, _, err := c.GetBook(ctx, bookID)
			if err != nil {
				Log(ctx).Warn("problem getting book for author", "authorID", authorID, "bookID", bookID, "err", err)
				continue
			}
			var w workResource
			_ = json.Unmarshal(bookBytes, &w)

			if len(w.Authors) > 0 && w.Authors[0].ForeignID != authorID {
				Log(ctx).Debug("skipping edition due to author mismatch", "authorID", authorID, "got", w.Authors[0].ForeignID)
				continue
			}

			workID := w.ForeignID
			if _, _, err := c.GetWork(ctx, workID); err == nil { // Ensure fetched before denormalizing.
				workIDSToDenormalize = append(workIDSToDenormalize, workID)
			}
			n++
		}

		slices.Sort(workIDSToDenormalize)
		workIDSToDenormalize = slices.Compact(workIDSToDenormalize)

		// Don't block so we can free up the refresh group for someone else.
		go func() {
			if len(workIDSToDenormalize) > 0 {
				c.add(edge{kind: authorEdge, parentID: authorID, childIDs: newSet(workIDSToDenormalize...)})
			}
			c.add(edge{kind: refreshDone, parentID: authorID})
		}()
		Log(ctx).Info("fetched all works for author", "authorID", authorID, "count", len(workIDSToDenormalize), "duration", time.Since(start).String())

		return nil
	})
}

// Run is responsible for denormalizing data. Race conditions are still
// possible but less likely by serializing updates this way.
func (c *Controller) Run(ctx context.Context, wait time.Duration) {
	for edge := range c.groupEdges() {
		ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
		ctx = context.WithValue(ctx, middleware.RequestIDKey, fmt.Sprintf("denorm-%d-%d", edge.kind, edge.parentID))

		switch edge.kind {
		case authorEdge:
			if unknownAuthor(edge.parentID) {
				break
			}
			if err := c.denormalizeWorks(ctx, edge.parentID, slices.Collect(maps.Keys(edge.childIDs))...); err != nil {
				Log(ctx).Warn("problem ensuring work", "err", err, "authorID", edge.parentID, "workIDs", edge.childIDs)
			}
		case workEdge:
			if err := c.denormalizeEditions(ctx, edge.parentID, slices.Collect(maps.Keys(edge.childIDs))...); err != nil {
				Log(ctx).Warn("problem ensuring edition", "err", err, "workID", edge.parentID, "bookIDs", edge.childIDs)
			}
		case refreshDone:
			c.metrics.refreshWaitingAdd(-1)
			if err := c.persister.Delete(ctx, edge.parentID); err != nil {
				Log(ctx).Warn("problem un-persisting refresh", "err", err)
			}
		}
		cancel()
	}
}

// groupEdges peeks at edges before they get de-duped so we can accurately
// update our counters.
func (c *Controller) groupEdges() iter.Seq[edge] {
	ch := make(chan edge)
	go func() {
		for e := range c.denormC {
			ch <- e
		}
	}()
	return c.grouper.group(ch)
}

// add adds an edge to the denormalization queue and updates our counter.
func (c *Controller) add(e edge) {
	c.denormC <- e
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
	err = sonic.ConfigStd.NewDecoder(r).Decode(&work)
	if err != nil {
		Log(ctx).Debug("problem unmarshaling work", "err", err, "workID", workID)
		_ = c.cache.Expire(ctx, WorkKey(workID))
		return err
	}

	Log(ctx).Debug("ensuring work-edition edges", "workID", workID, "bookIDs", bookIDs)

	for _, bookID := range bookIDs {
		workBytes, _, _, err = c.getter.GetBook(ctx, bookID, nil)
		if err != nil {
			// Maybe the cache wasn't able to refresh because it was deleted? Move on.
			Log(ctx).Warn("unable to denormalize edition", "err", err, "workID", workID, "bookID", bookID)
			continue
		}
		var w workResource
		err = sonic.ConfigStd.Unmarshal(workBytes, &w)
		if err != nil {
			Log(ctx).Warn("problem unmarshaling book", "err", err, "bookID", bookID)
			_ = c.cache.Expire(ctx, BookKey(bookID))
			continue
		}
		if len(w.Books) != 1 {
			Log(ctx).Warn("unexpected number of books", "bookID", bookID, "count", len(w.Books))
			continue
		}

		// GetBook can return a merged book/edition with an ID not matching
		// bookID, and that's the ID we need to probe for.
		bookID = w.Books[0].ForeignID

		idx, found := slices.BinarySearchFunc(work.Books, bookID, func(b bookResource, id int64) int {
			return cmp.Compare(b.ForeignID, id)
		})

		if found {
			work.Books[idx] = w.Books[0] // Replace.
		} else {
			work.Books = slices.Insert(work.Books, idx, w.Books[0]) // Insert.
		}
	}

	buf := _buffers.Get()
	defer buf.Free()
	neww := newETagWriter()
	w := io.MultiWriter(buf, neww)
	err = sonic.ConfigStd.NewEncoder(w).Encode(work)
	if err != nil {
		return err
	}

	if neww.ETag() == old.ETag() {
		// The work didn't change, so we're done.
		c.metrics.etagMatchesInc()
		return nil
	}
	c.metrics.etagMismatchesInc()

	// We can't persist the shared buffer in the cache so clone it.
	out := bytes.Clone(buf.Bytes())

	c.cache.Set(ctx, WorkKey(workID), out, fuzz(_workTTL, 1.5))

	// We modified the work, so the author also needs to be updated. Remove the
	// relationship so it doesn't no-op during the denormalization.
	go func() {
		for _, author := range work.Authors {
			c.add(edge{kind: authorEdge, parentID: author.ForeignID, childIDs: newSet(workID)})
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
	err = sonic.ConfigStd.NewDecoder(r).Decode(&author)
	if err != nil {
		Log(ctx).Debug("problem unmarshaling author", "err", err, "authorID", authorID)
		_ = c.cache.Expire(ctx, AuthorKey(authorID))
		return err
	}

	Log(ctx).Debug("ensuring author-work edges", "authorID", authorID, "workIDs", workIDs)

	for _, workID := range workIDs {
		workBytes, _, err := c.getter.GetWork(ctx, workID, nil)
		if err != nil {
			// Maybe the cache wasn't able to refresh because it was deleted? Move on.
			Log(ctx).Warn("unable to denormalize work", "err", err, "authorID", authorID, "workID", workID)
			continue
		}
		var work workResource
		err = sonic.ConfigStd.Unmarshal(workBytes, &work)
		if err != nil {
			Log(ctx).Warn("problem unmarshaling work", "err", err, "workID", workID)
			_ = c.cache.Expire(ctx, WorkKey(workID))
			continue
		}
		workID = work.ForeignID // GetWork can return a merged work with a different ID.

		idx, found := slices.BinarySearchFunc(author.Works, workID, func(w workResource, id int64) int {
			return cmp.Compare(w.ForeignID, id)
		})

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

	author.Series = []SeriesResource{}

	wg := sync.WaitGroup{}
	mu := sync.Mutex{}

	// Keep track of any duplicated titles so we can disambiguate them with subtitles.
	titles := map[string]int{}

	ratingSum := int64(0)
	ratingCount := int64(0)
	for _, w := range author.Works {
		if w.ShortTitle != "" {
			titles[strings.ToUpper(w.ShortTitle)]++
		} else {
			titles[strings.ToUpper(w.Title)]++
		}
		// HC stores rating on the work while GR is on the edition.
		ratingCount += w.RatingCount
		ratingSum += w.RatingSum
		if w.RatingCount == 0 {
			for _, b := range w.Books {
				ratingCount += b.RatingCount
				ratingSum += b.RatingSum
			}
		}
		for _, s := range w.Series {
			// Fetch the complete series since we might not derive it correctly from works alone.
			wg.Add(1)
			go func() {
				defer wg.Done()

				s, err := c.GetSeries(ctx, s.ForeignID)
				if err != nil {
					return
				}

				var ss SeriesResource
				err = json.Unmarshal(s, &ss)
				if err != nil {
					return
				}

				mu.Lock()
				defer mu.Unlock()

				idx, found := slices.BinarySearchFunc(author.Series, ss.ForeignID, func(s SeriesResource, id int64) int {
					return cmp.Compare(s.ForeignID, id)
				})

				if !found {
					author.Series = slices.Insert(author.Series, idx, ss)
				}
			}()
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
	if ratingCount != 0 {
		author.RatingCount = ratingCount
		author.AverageRating = float32(ratingSum) / float32(ratingCount)
	}

	wg.Wait()

	buf := _buffers.Get()
	defer buf.Free()
	neww := newETagWriter()
	w := io.MultiWriter(buf, neww)
	err = sonic.ConfigStd.NewEncoder(w).Encode(author)
	if err != nil {
		return err
	}

	if neww.ETag() == old.ETag() {
		// The author didn't change, so we're done.
		c.metrics.etagMatchesInc()
		return nil
	}
	c.metrics.etagMismatchesInc()

	// We can't persist the shared buffer in the cache so clone it.
	out := bytes.Clone(buf.Bytes())

	c.cache.Set(ctx, AuthorKey(authorID), out, fuzz(_authorTTL, 1.5))

	return nil
}

// editionsCallback can be used by a Getter to trigger async loading of
// additional editions.
type editionsCallback func(...workResource)

// fuzz scales the given duration into the range (d, d * f).
func fuzz(d time.Duration, f float64) time.Duration {
	if f < 1.0 {
		f += 1.0
	}
	factor := 1.0 + rand.Float64()*(f-1.0)
	return time.Duration(float64(d) * factor)
}

type ttlpair struct {
	bytes []byte
	ttl   time.Duration
}

// Configure sonic's memory pooling.
func init() {
	option.LimitBufferSize = 100 * 1024 * 1024    // 100MB max buffer.
	option.DefaultDecoderBufferSize = 1024 * 1024 // 1MB
	option.DefaultEncoderBufferSize = 1024 * 1024 // 1MB
}
