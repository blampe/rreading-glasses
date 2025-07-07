package internal

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// strip all `{...}` segments from the pattern to build a label
	pathParamRE = regexp.MustCompile(`\{[^/]+\}`)

	_ HTTPMetrics       = (*httpMetrics)(nil)
	_ HTTPMetrics       = (*nohttpmetrics)(nil)
	_ ControllerMetrics = (*controllerMetrics)(nil)
	_ ControllerMetrics = (*noControllerMetrics)(nil)
	_ CacheMetrics      = (*cacheMetrics)(nil)
	_ CacheMetrics      = (*noCacheMetrics)(nil)
	_ GQLMetrics        = (*nogqlMetrics)(nil)
	_ GQLMetrics        = (*gqlMetrics)(nil)
)

type HTTPMetrics interface {
	HandleFunc(mux *http.ServeMux, pattern string, hf http.HandlerFunc)
}

type httpMetrics struct {
	requestDuration *prometheus.HistogramVec
	requestInflight prometheus.Gauge
}

type nohttpmetrics struct{}

type ControllerMetrics interface {
	DenormWaitingInc()
	DenormWaitingDec()
	DenormWaitingAdd(delta int64)
	RefreshWaitingInc()
	RefreshWaitingDec()
	RefreshWaitingAdd(delta int64)
	EtagMatchesInc()
	EtagMismatchesInc()
	EtagRatioSet(val float64)
}

type controllerMetrics struct {
	controllerTotals  *prometheus.CounterVec
	controllerWaiting *prometheus.GaugeVec
	eTagTotals        *prometheus.CounterVec
	eTagRatio         prometheus.Gauge
}

type noControllerMetrics struct{}

type CacheMetrics interface {
	CacheHitInc()
	CacheMissInc()
	CacheHitRatioSet(val float64)
}

type cacheMetrics struct {
	totals   *prometheus.CounterVec
	hitRatio prometheus.Gauge
}

type noCacheMetrics struct{}

type GQLMetrics interface {
	BatchesSentInc()
	QueriesSentInc()
	QueriesSentAdd(delta int64)
}

type gqlMetrics struct {
	totals *prometheus.CounterVec
}

type nogqlMetrics struct{}

type MetricsMiddleware struct {
	reg        *prometheus.Registry
	HTTP       HTTPMetrics
	Controller ControllerMetrics
	Cache      CacheMetrics
	GQL        GQLMetrics
}

func NewPrometheusMetrics(appName string) MetricsMiddleware {
	reg := prometheus.NewRegistry()

	// reg.MustRegister(
	// 	collectors.NewGoCollector(),
	// 	collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	// )

	httpRequestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: appName,
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request latencies by method & path",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"method", "path", "status"},
	)

	inFlight := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: appName,
			Subsystem: "http",
			Name:      "requests_inflight",
			Help:      "Current number of inbound in-flight HTTP requests.",
		},
	)

	// Controller Metrics
	controllerTotalOps := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: appName,
			Subsystem: "controller",
			Name:      "total_operations",
			Help:      "Counts of total controller operations by type.",
		},
		[]string{"type"},
	)
	controllerWaitingOps := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: appName,
			Subsystem: "controller",
			Name:      "waiting_operations",
			Help:      "Counts of waiting controller operations by type.",
		},
		[]string{"type"},
	)
	controllerEtag := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: appName,
			Subsystem: "controller",
			Name:      "etag_total",
			Help:      "Counts of controller operations by type.",
		},
		[]string{"type"},
	)

	controllerETagRatio := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: appName,
			Subsystem: "controller",
			Name:      "etag_hit_ratio",
			Help:      "ETag hit ratio.",
		},
	)

	// Cache Metrics
	cacheHits := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: appName,
			Subsystem: "cache",
			Name:      "total",
			Help:      "Totals for cache system.",
		},
		[]string{"type"},
	)
	cacheHitRatio := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: appName,
			Subsystem: "cache",
			Name:      "hit_ratio",
			Help:      "Ratio of cache hits to total cache operations.",
		},
	)
	gqlclientCounters := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: appName,
			Subsystem: "gqlclient",
			Name:      "total",
			Help:      "How many batches have been sent.",
		},
		[]string{"type"},
	)

	// Register all
	reg.MustRegister(
		httpRequestDuration,
		inFlight,

		controllerTotalOps,
		controllerWaitingOps,
		controllerEtag,
		controllerETagRatio,

		cacheHits,
		cacheHitRatio,

		gqlclientCounters,
	)

	return MetricsMiddleware{
		reg: reg,
		HTTP: &httpMetrics{
			requestDuration: httpRequestDuration,
			requestInflight: inFlight,
		},
		Controller: &controllerMetrics{
			controllerTotals:  controllerTotalOps,
			controllerWaiting: controllerWaitingOps,
			eTagTotals:        controllerEtag,
			eTagRatio:         controllerETagRatio,
		},
		Cache: &cacheMetrics{
			totals:   cacheHits,
			hitRatio: cacheHitRatio,
		},
		GQL: &gqlMetrics{
			totals: gqlclientCounters,
		},
	}
}

func (hm *httpMetrics) HandleFunc(mux *http.ServeMux, pattern string, hf http.HandlerFunc) {
	// derive the constant label from the pattern:
	//   "/author/{foreignAuthorID}" → "/author"
	//   "/book/bulk"                → "/book/bulk"
	label := normalizePattern(pattern)

	wrapped := func(w http.ResponseWriter, r *http.Request) {
		hm.requestInflight.Inc()
		defer hm.requestInflight.Dec()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		hf(rw, r)

		dur := time.Since(start).Seconds()
		hm.requestDuration.
			WithLabelValues(r.Method, label, strconv.Itoa(rw.status)).
			Observe(dur)
	}

	mux.HandleFunc(pattern, wrapped)
}

func normalizePattern(pattern string) string {
	p := strings.TrimSuffix(pattern, "/")
	p = pathParamRE.ReplaceAllString(p, "")
	p = strings.ReplaceAll(p, "//", "/")
	return p
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (m *MetricsMiddleware) PrometheusHandler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

func (*nohttpmetrics) HandleFunc(mux *http.ServeMux, pattern string, hf http.HandlerFunc) {
	mux.HandleFunc(pattern, hf)
}

func (cm *controllerMetrics) DenormWaitingInc() {
	cm.controllerWaiting.WithLabelValues("denorm_waiting").Inc()
}

func (cm *controllerMetrics) DenormWaitingDec() {
	cm.controllerWaiting.WithLabelValues("denorm_waiting").Dec()
	cm.denormCompletedInc()
}

func (cm *controllerMetrics) DenormWaitingAdd(delta int64) {
	if delta == 0 {
		return
	}
	if delta < 0 {
		cm.controllerWaiting.WithLabelValues("denorm_waiting").Sub(float64(-delta))
		cm.denormCompletedAdd(-delta)
	} else {
		cm.controllerWaiting.WithLabelValues("denorm_waiting").Add(float64(delta))
	}
}

func (cm *controllerMetrics) denormCompletedInc() {
	cm.controllerTotals.WithLabelValues("denorm_completed").Inc()
}

func (cm *controllerMetrics) denormCompletedAdd(delta int64) {
	if delta <= 0 {
		return
	} else {
		cm.controllerTotals.WithLabelValues("denorm_completed").Add(float64(delta))
	}
}

func (cm *controllerMetrics) RefreshWaitingInc() {
	cm.controllerWaiting.WithLabelValues("refresh_waiting").Inc()
}

func (cm *controllerMetrics) RefreshWaitingDec() {
	cm.controllerWaiting.WithLabelValues("refresh_waiting").Dec()
	cm.refreshCompletedInc()
}

func (cm *controllerMetrics) RefreshWaitingAdd(delta int64) {
	if delta == 0 {
		return
	}
	if delta < 0 {
		cm.controllerWaiting.WithLabelValues("refresh_waiting").Sub(float64(-delta))
		cm.refreshCompletedAdd(-delta)
	} else {
		cm.controllerWaiting.WithLabelValues("refresh_waiting").Add(float64(delta))
	}
}

func (cm *controllerMetrics) refreshCompletedInc() {
	cm.controllerTotals.WithLabelValues("refresh_completed").Inc()
}

func (cm *controllerMetrics) refreshCompletedAdd(delta int64) {
	if delta <= 0 {
		return
	} else {
		cm.controllerTotals.WithLabelValues("refresh_completed").Add(float64(delta))
	}
}

func (cm *controllerMetrics) EtagMatchesInc() {
	cm.eTagTotals.WithLabelValues("matches").Inc()
}
func (cm *controllerMetrics) EtagMismatchesInc() {
	cm.eTagTotals.WithLabelValues("mismatches").Inc()
}
func (cm *controllerMetrics) EtagRatioSet(val float64) {
	cm.eTagRatio.Set(val)
}

func (cm *noControllerMetrics) DenormWaitingInc()             {}
func (cm *noControllerMetrics) DenormWaitingDec()             {}
func (cm *noControllerMetrics) DenormWaitingAdd(delta int64)  {}
func (cm *noControllerMetrics) RefreshWaitingInc()            {}
func (cm *noControllerMetrics) RefreshWaitingDec()            {}
func (cm *noControllerMetrics) RefreshWaitingAdd(delta int64) {}
func (cm *noControllerMetrics) EtagMatchesInc()               {}
func (cm *noControllerMetrics) EtagMismatchesInc()            {}
func (cm *noControllerMetrics) EtagRatioSet(val float64)      {}

func (cm *cacheMetrics) CacheHitInc() {
	cm.totals.WithLabelValues("hits").Inc()
}
func (cm *cacheMetrics) CacheMissInc() {
	cm.totals.WithLabelValues("misses").Inc()
}
func (cm *cacheMetrics) CacheHitRatioSet(val float64) {
	cm.hitRatio.Set(val)
}

func (cm *noCacheMetrics) CacheHitInc()                 {}
func (cm *noCacheMetrics) CacheMissInc()                {}
func (cm *noCacheMetrics) CacheHitRatioSet(val float64) {}

func (gm *gqlMetrics) BatchesSentInc() {
	gm.totals.WithLabelValues("batches_sent").Inc()
}

func (gm *gqlMetrics) QueriesSentInc() {
	gm.totals.WithLabelValues("queries_sent").Inc()
}

func (gm *gqlMetrics) QueriesSentAdd(delta int64) {
	if delta <= 0 {
		return
	} else {
		gm.totals.WithLabelValues("queries_sent").Add(float64(delta))
	}
}

func (gm *nogqlMetrics) BatchesSentInc()            {}
func (gm *nogqlMetrics) QueriesSentInc()            {}
func (gm *nogqlMetrics) QueriesSentAdd(delta int64) {}
