package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

// Use lower values while we're beta testing.
var (
	_authorTTL = 3 * 24 * time.Hour // 3 days.
	// _authorTTL  = 30 * 24 * time.Hour     // 1 month.

	_workTTL = 7 * 24 * time.Hour // 1 week.
	// _workTTL    = 30 * 24 * time.Hour     // 1 month.

	_editionTTL = 30 * 24 * time.Hour // 1 month.
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
	cache *layeredcache
	core  getter             // Core GetBook/GetAuthor/GetWork implementation.
	group singleflight.Group // Coalesce lookups for the same key.

	// cf       *cloudflare.API // TODO: CDN invalidation.
}

// getter allows alternative implementations of the core logic to be injected.
// Don't write to the cache if you use it.
type getter interface {
	GetWork(ctx context.Context, workID int64) ([]byte, error)
	GetAuthor(ctx context.Context, authorID int64) ([]byte, error)
	GetBook(ctx context.Context, bookID int64) ([]byte, error) // Returns a serialized Work??
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

func newController(cache *layeredcache, core getter) (*controller, error) {
	c := &controller{
		cache: cache,
		core:  core,
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
	workBytes, err := c.core.GetBook(ctx, bookID)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, bookKey(bookID), _missing, _editionTTL)
		return nil, err
	}
	if err != nil {
		log(ctx).Warn("problem getting book", "err", err, "bookID", bookID)
		return nil, err
	}

	c.cache.Set(ctx, bookKey(bookID), workBytes, _editionTTL)

	return workBytes, nil
}

func (c *controller) getWork(ctx context.Context, workID int64) ([]byte, error) {
	workBytes, ok := c.cache.Get(ctx, workKey(workID))
	if slices.Equal(workBytes, _missing) {
		return nil, errNotFound
	}
	if ok {
		return workBytes, nil
	}

	// Cache miss.
	workBytes, err := c.core.GetWork(ctx, workID)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, workKey(workID), _missing, _workTTL)
		return nil, err
	}
	if err != nil {
		log(ctx).Warn("problem getting work", "err", err, "workID", workID)
	}

	c.cache.Set(ctx, workKey(workID), workBytes, _workTTL)

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

	authorBytes, ttl, ok := c.cache.GetWithTTL(ctx, authorKey(authorID))
	if slices.Equal(authorBytes, _missing) {
		return nil, errNotFound
	}

	// Our local TTL is 2*_authorTTL, but we want the client to see a miss
	// after _authorTTL.
	if ok && ttl > _authorTTL {
		return authorBytes, nil
	}

	log(ctx).Debug("refreshing", "authorID", authorID, "ttl", ttl)

	authorBytes, err := c.core.GetAuthor(ctx, authorID)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, authorKey(authorID), _missing, _authorTTL)
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	c.cache.Set(ctx, authorKey(authorID), authorBytes, 2*_authorTTL)

	return authorBytes, nil
}
