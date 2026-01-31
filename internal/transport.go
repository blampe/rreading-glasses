package internal

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
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

// LoggingTransport logs HTTP requests and responses with rate limit information.
// This is useful for debugging API interactions and understanding batching behavior.
type LoggingTransport struct {
	http.RoundTripper
}

// RoundTrip logs the HTTP request and response details including rate limit headers.
func (t *LoggingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	
	// Get request ID from context for correlation
	ctx := r.Context()
	requestID := middleware.GetReqID(ctx)
	if requestID == "" {
		requestID = "unknown"
	}

	// Log the outgoing request
	Log(ctx).Debug("HTTP request to Hardcover",
		"requestID", requestID,
		"method", r.Method,
		"url", r.URL.String(),
	)

	// Make the actual request
	resp, err := t.RoundTripper.RoundTrip(r)
	
	duration := time.Since(start)

	if err != nil {
		Log(ctx).Debug("HTTP request failed",
			"requestID", requestID,
			"method", r.Method,
			"url", r.URL.String(),
			"duration", duration,
			"error", err,
		)
		return nil, err
	}

	// Extract rate limit headers if present
	rateLimitRemaining := resp.Header.Get("X-RateLimit-Remaining")
	rateLimitLimit := resp.Header.Get("X-RateLimit-Limit")
	rateLimitReset := resp.Header.Get("X-RateLimit-Reset")
	retryAfter := resp.Header.Get("Retry-After")

	// Log the response with all rate limit information
	logArgs := []any{
		"requestID", requestID,
		"method", r.Method,
		"url", r.URL.String(),
		"status", resp.StatusCode,
		"duration", duration,
	}

	// Add rate limit info if headers are present
	if rateLimitRemaining != "" {
		logArgs = append(logArgs, "rateLimitRemaining", rateLimitRemaining)
	}
	if rateLimitLimit != "" {
		logArgs = append(logArgs, "rateLimitLimit", rateLimitLimit)
	}
	if rateLimitReset != "" {
		logArgs = append(logArgs, "rateLimitReset", rateLimitReset)
	}
	if retryAfter != "" {
		logArgs = append(logArgs, "retryAfter", retryAfter)
	}

	Log(ctx).Debug("HTTP response from Hardcover", logArgs...)

	return resp, nil
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
