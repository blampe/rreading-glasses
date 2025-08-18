package internal

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestInstrument(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	notFoundGetter := NewMockgetter(gomock.NewController(t))
	notFoundGetter.EXPECT().GetAuthor(gomock.Any(), int64(123)).Return(nil, errNotFound).AnyTimes()

	ctrl, err := NewController(newMemoryCache(), notFoundGetter, nil, reg)
	require.NoError(t, err)

	h := NewHandler(ctrl)
	mux := NewMux(h, reg)

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/author/123")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, err = http.Get(ts.URL + "/debug/metrics")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Contains(t, string(got), `http_inflight 1`)
	assert.Contains(t, string(got), `http_requests_bucket{method="GET",path="/author",status="404",le="1"} 1`)
}

func TestControllerMetrics(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	cm := newControllerMetrics(reg)

	// Simulate denorm flow
	cm.denormWaitingAdd(2)
	cm.denormWaitingAdd(-2)

	// Simulate refresh flow
	cm.refreshWaitingAdd(3)
	cm.refreshWaitingAdd(-3)

	// ETag matches/mismatches
	cm.etagMatchesInc()
	cm.etagMismatchesInc()

	assert.Equal(t, 0.0, testutil.ToFloat64(cm.totals.WithLabelValues("denormalization")))
	assert.Equal(t, 0.0, testutil.ToFloat64(cm.totals.WithLabelValues("refresh")))
	assert.Equal(t, 1.0, testutil.ToFloat64(cm.totals.WithLabelValues("etag_matches")))
	assert.Equal(t, 1.0, testutil.ToFloat64(cm.totals.WithLabelValues("etag_mismatches")))
}

func TestCacheMetrics(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	cm := newCacheMetrics(reg)

	cm.cacheHitInc()
	cm.cacheMissInc()

	assert.Equal(t, 1.0, testutil.ToFloat64(cm.totals.WithLabelValues("hits")))
	assert.Equal(t, 1.0, testutil.ToFloat64(cm.totals.WithLabelValues("misses")))
	assert.Equal(t, 0.5, cm.cacheHitRatioGet())
}

func TestNormalizePattern(t *testing.T) {
	assert.Equal(t, "/author", normalizePattern("/author/{foreignAuthorID}"))
	assert.Equal(t, "/book/bulk", normalizePattern("/book/bulk/"))
}
