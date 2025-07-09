package internal

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestHTTPMetrics_HandleFunc(t *testing.T) {
	mm := NewPrometheusMetrics("test")

	mux := http.NewServeMux()

	mm.HTTP.HandleFunc(mux, "/author/{foreignAuthorID}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // 418
		_, _ = w.Write([]byte("OK"))
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/author/123")
	assert.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "OK", string(body))
	assert.Equal(t, http.StatusTeapot, resp.StatusCode)

	assert.Equal(t, 0.0, testutil.ToFloat64(mm.HTTP.(*httpMetrics).requestInflight))

	count := testutil.CollectAndCount(mm.HTTP.(*httpMetrics).requestDuration)
	assert.True(t, count > 0)
}

func TestControllerMetrics(t *testing.T) {
	mm := NewPrometheusMetrics("test")

	cm := mm.Controller.(*controllerMetrics)

	// Simulate denorm flow
	cm.DenormWaitingInc()
	cm.DenormWaitingDec()
	cm.DenormWaitingAdd(2)
	cm.DenormWaitingAdd(-2)

	// Simulate refresh flow
	cm.RefreshWaitingInc()
	cm.RefreshWaitingDec()
	cm.RefreshWaitingAdd(3)
	cm.RefreshWaitingAdd(-3)

	// ETag matches/mismatches
	cm.EtagMatchesInc()
	cm.EtagMismatchesInc()
	cm.EtagRatioSet(0.75)

	assert.Equal(t, 3.0, testutil.ToFloat64(cm.controllerTotals.WithLabelValues("denorm_completed")))
	assert.Equal(t, 4.0, testutil.ToFloat64(cm.controllerTotals.WithLabelValues("refresh_completed")))
	assert.Equal(t, 0.75, testutil.ToFloat64(cm.eTagRatio))
	assert.Equal(t, 1.0, testutil.ToFloat64(cm.eTagTotals.WithLabelValues("matches")))
	assert.Equal(t, 1.0, testutil.ToFloat64(cm.eTagTotals.WithLabelValues("mismatches")))
}

func TestCacheMetrics(t *testing.T) {
	mm := NewPrometheusMetrics("test")
	cm := mm.Cache.(*cacheMetrics)

	cm.CacheHitInc()
	cm.CacheMissInc()
	cm.CacheHitRatioSet(0.8)

	assert.Equal(t, 1.0, testutil.ToFloat64(cm.totals.WithLabelValues("hits")))
	assert.Equal(t, 1.0, testutil.ToFloat64(cm.totals.WithLabelValues("misses")))
	assert.Equal(t, 0.8, testutil.ToFloat64(cm.hitRatio))
}

func TestNormalizePattern(t *testing.T) {
	assert.Equal(t, "/author", normalizePattern("/author/{foreignAuthorID}"))
	assert.Equal(t, "/book/bulk", normalizePattern("/book/bulk/"))
}
