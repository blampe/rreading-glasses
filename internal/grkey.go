package internal

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// grKeyPage is any public Goodreads page whose embedded Next.js state carries
// the AppSync API key the site's own frontend uses.
const grKeyPage = "https://www.goodreads.com/book/show/1"

var _grAPIKey = regexp.MustCompile(`"apiKey":"([A-Za-z0-9-]{20,64})"`)

// grKeyTransport sets the AppSync X-Api-Key header and, when upstream answers
// 401, re-reads the current public key from a Goodreads page and retries once.
//
// The key baked into NewGRGQL is the one Goodreads served when it was written.
// Goodreads rotated it in Aug 2026 (upstream issue #586) and every GR instance
// returned 401 for every lookup until someone edited the source. Reading the
// key from the same page a browser loads makes the next rotation self-heal.
type grKeyTransport struct {
	base    http.RoundTripper // must return statusErr for >= 400
	fetch   func() (string, error)
	minWait time.Duration

	mu        sync.Mutex
	key       string
	lastFetch time.Time
}

func newGRKeyTransport(initial string, base http.RoundTripper) *grKeyTransport {
	return &grKeyTransport{base: base, key: initial, fetch: fetchGRKey, minWait: 5 * time.Minute}
}

func (t *grKeyTransport) current() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.key
}

// refresh fetches a new key unless another caller already replaced `stale` or
// a fetch happened within minWait. Returns the key to retry with, or "" if
// there is nothing new to try.
func (t *grKeyTransport) refresh(stale string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.key != stale {
		return t.key // someone else already refreshed
	}
	if time.Since(t.lastFetch) < t.minWait {
		return ""
	}
	t.lastFetch = time.Now()
	k, err := t.fetch()
	if err != nil {
		slog.Warn("refreshing GR AppSync key failed", "err", err)
		return ""
	}
	if k == t.key {
		slog.Warn("GR AppSync key unchanged after 401; upstream may have changed auth scheme")
		return ""
	}
	slog.Info("GR AppSync key rotated; using key from Goodreads page")
	t.key = k
	return k
}

func (t *grKeyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	key := t.current()
	resp, err := t.send(r, key)
	var se statusErr
	if err == nil || !errors.As(err, &se) || int(se) != http.StatusUnauthorized {
		return resp, err
	}
	next := t.refresh(key)
	if next == "" {
		return resp, err
	}
	if r.Body != nil && r.GetBody == nil {
		return resp, err // can't replay the body
	}
	return t.send(r, next)
}

func (t *grKeyTransport) send(r *http.Request, key string) (*http.Response, error) {
	req := r.Clone(r.Context())
	if r.GetBody != nil {
		body, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		req.Body = body
	}
	req.Header.Set("X-Api-Key", key)
	return t.base.RoundTrip(req)
}

func fetchGRKey() (string, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, grKeyPage, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", grKeyPage, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	m := _grAPIKey.FindSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("no apiKey in %s", grKeyPage)
	}
	return string(m[1]), nil
}
