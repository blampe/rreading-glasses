package internal

import (
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

// throttledTransport rate limits requests.
type throttledTransport struct {
	http.RoundTripper
	*rate.Limiter
}

func (t throttledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.Limiter.Wait(r.Context()); err != nil {
		return nil, err
	}
	resp, err := t.RoundTripper.RoundTrip(r)

	// Back off for a minute if we got a 403.
	// TODO: Return a Retry-After: (seconds) response header..
	if resp != nil && resp.StatusCode == http.StatusForbidden {
		slog.Default().Warn("backing off after 403", "limit", t.Limiter.Limit(), "tokens", t.Limiter.Tokens())
		orig := t.Limiter.Limit()
		t.Limiter.SetLimit(rate.Every(time.Hour / 60))          // 1RPM
		t.Limiter.SetLimitAt(time.Now().Add(time.Minute), orig) // Restore
	}

	return resp, err
}

// ScopedTransport restricts requests to a particular host.
type ScopedTransport struct {
	Host string
	http.RoundTripper
}

// RoundTrip forces the request to stick to the given host, so redirects can't
// send us elsewhere. Helpful to ensuring credentials don't leak to other
// domains.
func (t ScopedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme = "https"
	r.URL.Host = t.Host
	return t.RoundTripper.RoundTrip(r)
}

// cookieTransport transport adds a cookie to all requests. Best used with a
// scopedTransport.
type cookieTransport struct {
	cookies []*http.Cookie
	http.RoundTripper
}

func (t cookieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	for _, c := range t.cookies {
		r.AddCookie(c)
	}
	return t.RoundTripper.RoundTrip(r)
}

// HeaderTransport adds a header to all requests. Best used with a
// scopedTransport.
type HeaderTransport struct {
	Key   string
	Value string
	http.RoundTripper
}

// RoundTrip always sets the header on the request.
func (t HeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Add(t.Key, t.Value)
	return t.RoundTripper.RoundTrip(r)
}

// errorProxyTransport returns a non-nil statusErr for all response codes 400
// and above so we can return a response with the same code.
type errorProxyTransport struct {
	http.RoundTripper
}

// RoundTrip wraps upstream 4XX and 5XX errors such that they are returned
// directly to the client.
func (t errorProxyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, statusErr(resp.StatusCode)
	}
	return resp, nil
}
