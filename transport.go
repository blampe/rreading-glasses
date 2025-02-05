package main

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

// scopedTransport restricts requests to a particular host.
type scopedTransport struct {
	host string
	http.RoundTripper
}

func (t scopedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme = "https"
	r.URL.Host = t.host
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

// authTransport adds an Authorization header to all requests. Best used with a
// scopedTransport.
type authTransport struct {
	header string
	http.RoundTripper
}

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Add("authorization", t.header)
	return t.RoundTripper.RoundTrip(r)
}

// errorProxyTransport returns a non-nil statusErr for all response codes 400
// and above so we can return a response with the same code.
type errorProxyTransport struct {
	http.RoundTripper
}

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
