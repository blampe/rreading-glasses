package internal

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

type fakeRT struct {
	good  string
	calls int
	seen  []string
}

func (f *fakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	b, _ := io.ReadAll(r.Body)
	f.seen = append(f.seen, r.Header.Get("X-Api-Key")+"|"+string(b))
	if r.Header.Get("X-Api-Key") != f.good {
		return nil, statusErr(http.StatusUnauthorized)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func newReq(t *testing.T) *http.Request {
	r, err := http.NewRequest(http.MethodPost, "https://example.invalid/graphql", bytes.NewReader([]byte(`{"q":1}`)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGRKeyRefreshOn401(t *testing.T) {
	rt := &fakeRT{good: "new"}
	fetches := 0
	tr := newGRKeyTransport("old", rt)
	tr.fetch = func() (string, error) { fetches++; return "new", nil }

	if _, err := tr.RoundTrip(newReq(t)); err != nil {
		t.Fatalf("want success after refresh, got %v", err)
	}
	if fetches != 1 || rt.calls != 2 || rt.seen[1] != `new|{"q":1}` {
		t.Fatalf("fetches=%d calls=%d seen=%v", fetches, rt.calls, rt.seen)
	}
	// Subsequent requests use the new key directly.
	if _, err := tr.RoundTrip(newReq(t)); err != nil || rt.calls != 3 || fetches != 1 {
		t.Fatalf("err=%v calls=%d fetches=%d", err, rt.calls, fetches)
	}
}

func TestGRKeyRefreshRateLimited(t *testing.T) {
	rt := &fakeRT{good: "never"}
	fetches := 0
	tr := newGRKeyTransport("old", rt)
	tr.fetch = func() (string, error) { fetches++; return "other", nil }
	tr.minWait = time.Hour

	for i := 0; i < 5; i++ {
		_, err := tr.RoundTrip(newReq(t))
		var se statusErr
		if !errors.As(err, &se) || int(se) != 401 {
			t.Fatalf("want 401, got %v", err)
		}
	}
	if fetches != 1 {
		t.Fatalf("fetched %d times, want 1 (rate limited)", fetches)
	}
}

func TestGRKeyFetchFailureKeeps401(t *testing.T) {
	rt := &fakeRT{good: "new"}
	tr := newGRKeyTransport("old", rt)
	tr.fetch = func() (string, error) { return "", errors.New("boom") }
	_, err := tr.RoundTrip(newReq(t))
	var se statusErr
	if !errors.As(err, &se) || int(se) != 401 || rt.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, rt.calls)
	}
}
