package internal

import (
	"net/http"
	"time"
)

// throttledTransport rate limits requests.
type throttledTransport struct {
	http.RoundTripper
	ticker *time.Ticker
}

func (t throttledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-t.ticker.C:
		// allowed
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}

	return t.RoundTripper.RoundTrip(r)
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

// HeaderTransport adds a header to all requests. Best used with a
// scopedTransport.
type HeaderTransport struct {
	Key   string
	Value string
	http.RoundTripper
}

// RoundTrip always sets the header on the request.
func (t *HeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
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
