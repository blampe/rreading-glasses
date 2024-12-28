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
	if resp.StatusCode == http.StatusForbidden {
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
