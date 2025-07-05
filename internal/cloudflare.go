package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/time/rate"
)

var _ cache[[]byte] = (*CloudflareCache)(nil)

type cloudflareBuster struct {
	mu sync.Mutex

	url    string
	client *http.Client
	queue  map[string]struct{}
}

func newCloudflareBuster(apiToken string, zoneID string) (*cloudflareBuster, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/purge_cache", zoneID)

	client := &http.Client{
		Transport: throttledTransport{
			Limiter: rate.NewLimiter(rate.Limit(1), 1),
			RoundTripper: ScopedTransport{
				Host: "api.cloudflare.com",
				RoundTripper: &HeaderTransport{
					Key:          "Authorization",
					Value:        "Bearer " + apiToken,
					RoundTripper: http.DefaultTransport,
				},
			},
		},
	}
	cb := &cloudflareBuster{
		url:    url,
		client: client,
		queue:  map[string]struct{}{},
	}
	return cb, nil
}

func (cb *cloudflareBuster) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	for {
		select {
		case <-ticker.C:
			cb.flush(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (cb *cloudflareBuster) Add(url string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.queue[url] = struct{}{}
}

func (cb *cloudflareBuster) flush(ctx context.Context) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if len(cb.queue) == 0 {
		return
	}

	body := struct {
		Files []string `json:"files"`
	}{}

	inflight := []string{}

	for url := range maps.Keys(cb.queue) {
		if len(body.Files) >= 800 {
			break
		}
		body.Files = append(body.Files, url)
		inflight = append(inflight, url)
		delete(cb.queue, url)
	}

	Log(ctx).Debug("busting things", "count", len(body.Files))

	r, w := io.Pipe()
	defer func() { _ = w.Close() }()

	go func() {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		defer func() { _ = r.Close() }()

		// If we don't succeed then re-add the failed URLs to our queue.
		onError := func() {
			cb.mu.Lock()
			defer cb.mu.Unlock()
			for _, url := range inflight {
				cb.queue[url] = struct{}{}
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", cb.url, r)
		if err != nil {
			Log(ctx).Warn("problem requesting cloudflare", "err", err)
			onError()
			return
		}
		resp, err := cb.client.Do(req)
		if err != nil {
			Log(ctx).Warn("problem busting cloudflare", "err", err)
			onError()
			return
		}
		if resp.StatusCode != http.StatusOK {
			dump, _ := httputil.DumpResponse(resp, true)
			Log(ctx).Warn("unexpected cloudflare response", "dump", string(dump))
			onError()
			return
		}
	}()

	err := json.NewEncoder(w).Encode(body)
	if err != nil {
		Log(ctx).Warn("problem serializing cloudflare", "err", err)
	}
}

// CloudflareCache is a caching layer which no-ops except when new cache values
// are written, in which case a cache bust is queued with Cloudflare.
type CloudflareCache struct {
	cb     *cloudflareBuster
	pather func(string) string // Responsible for mapping cache keys to URLs for busting.
}

// NewCloudflareCache creates a new CloudflareCache.
func NewCloudflareCache(apiKey string, zoneID string, pather func(string) string) (*CloudflareCache, error) {
	cb, err := newCloudflareBuster(apiKey, zoneID)
	if err != nil {
		return nil, err
	}

	ctx := context.WithValue(context.Background(), middleware.RequestIDKey, "cloudflare")
	go cb.Run(ctx)

	return &CloudflareCache{cb: cb, pather: pather}, nil
}

// Expire busts the Cloudflare CDN by enqueuing the path to be busted.
func (cc *CloudflareCache) Expire(ctx context.Context, key string) error {
	cc.cb.Add(cc.pather(key))
	return nil
}

// Set queues a URL for busting.
func (cc *CloudflareCache) Set(ctx context.Context, key string, _ []byte, _ time.Duration) {
	_ = cc.Expire(ctx, key)
}

// Get is a no-op.
func (cc *CloudflareCache) Get(ctx context.Context, key string) ([]byte, bool) {
	return nil, false
}

// GetWithTTL is a no-op.
func (cc *CloudflareCache) GetWithTTL(ctx context.Context, key string) ([]byte, time.Duration, bool) {
	return nil, 0, false
}
