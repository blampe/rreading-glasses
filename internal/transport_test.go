package internal

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoggingTransport(t *testing.T) {
	// Create a test server that simulates Hardcover API with rate limit headers
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set rate limit headers like Hardcover would
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "45")
		w.Header().Set("X-RateLimit-Reset", "1706745600")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data": {}}`))
	}))
	defer ts.Close()

	// Create a logging transport
	transport := &LoggingTransport{
		RoundTripper: http.DefaultTransport,
	}

	client := &http.Client{Transport: transport}

	// Make a request
	resp, err := client.Get(ts.URL)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Read and verify response
	body, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.True(t, strings.Contains(string(body), "data"))
	resp.Body.Close()

	// The logging would have occurred during the request
	// In real usage, you'd see the debug logs with rate limit info
}

func TestLoggingTransportWithError(t *testing.T) {
	// Create a test server that returns an error
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": "rate limit exceeded"}`))
	}))
	defer ts.Close()

	transport := &LoggingTransport{
		RoundTripper: http.DefaultTransport,
	}

	client := &http.Client{Transport: transport}

	resp, err := client.Get(ts.URL)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	resp.Body.Close()

	// The logging would show rate limit headers including Retry-After
}
