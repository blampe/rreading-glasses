package internal

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
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

// HTTPMetrics defines the interface for wrapping the http handlers
// and collecting prometheus metrics for incoming http requests.
type HTTPMetrics interface {
	HandleFunc(mux *http.ServeMux, pattern string, hf http.HandlerFunc)
}

type httpMetrics struct {
	requestDuration *prometheus.HistogramVec
	requestInflight prometheus.Gauge
	totals          *prometheus.CounterVec
}

type nohttpmetrics struct{}

// ControllerMetrics defines the interface for collecting
// prometheus metrics for controller operations.
type ControllerMetrics interface {
	DenormWaitingInc()
	DenormWaitingDec()
	DenormWaitingAdd(delta int64)
	DenormWaitingGet() float64
	RefreshWaitingInc()
	RefreshWaitingDec()
	RefreshWaitingAdd(delta int64)
	RefreshWaitingGet() float64
	EtagMatchesInc()
	EtagMatchesGet() float64
	EtagMismatchesInc()
	EtagMismatchesGet() float64
	EtagRatioGet() float64
}

type controllerMetrics struct {
	controllerTotals  *prometheus.CounterVec
	controllerWaiting *prometheus.GaugeVec
	eTagTotals        *prometheus.CounterVec
}

type noControllerMetrics struct{}

// CacheMetrics defines the interface for collecting
// prometheus metrics for cache operations.
type CacheMetrics interface {
	CacheHitInc()
	CacheHitGet() int64
	CacheMissInc()
	CacheMissGet() int64
	CacheHitRatioGet() float64
	CacheBookTotalInc()
	CacheAuthorTotalInc()
	CacheWorkTotalInc()
	CacheUnknownTotalInc()
	CacheZeroTotalInc()
}

type cacheMetrics struct {
	totals *prometheus.CounterVec
}

type noCacheMetrics struct{}

// GQLMetrics defines the interface for collecting
// prometheus metrics for GraphQL client operations.
type GQLMetrics interface {
	BatchesSentInc()
	BatchesSentGet() int64
	QueriesSentInc()
	QueriesSentAdd(delta int64)
	QueriesSentGet() int64
}

type gqlMetrics struct {
	totals *prometheus.CounterVec
}

type nogqlMetrics struct{}

// MetricsMiddleware is a middleware that collects prometheus metrics
// for http requests, controller operations, cache operations, and GraphQL client operations.
type MetricsMiddleware struct {
	reg        *prometheus.Registry
	HTTP       HTTPMetrics
	Controller ControllerMetrics
	Cache      CacheMetrics
	GQL        GQLMetrics
}

// NewPrometheusMetrics creates a new MetricsMiddleware with Prometheus metrics
// for the given application name. It registers various metrics such as HTTP request durations,
// in-flight requests, controller operations, cache hits, and GraphQL client operations.
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

	totals := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: appName,
			Subsystem: "http",
			Name:      "total_requests",
			Help:      "Total number of HTTP requests by method & path.",
		},
		[]string{"method", "path", "status"},
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
		totals,

		controllerTotalOps,
		controllerWaitingOps,
		controllerEtag,

		cacheHits,

		gqlclientCounters,
	)

	return MetricsMiddleware{
		reg: reg,
		HTTP: &httpMetrics{
			requestDuration: httpRequestDuration,
			requestInflight: inFlight,
			totals:          totals,
		},
		Controller: &controllerMetrics{
			controllerTotals:  controllerTotalOps,
			controllerWaiting: controllerWaitingOps,
			eTagTotals:        controllerEtag,
		},
		Cache: &cacheMetrics{
			totals: cacheHits,
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

		hm.totals.WithLabelValues(r.Method, label, strconv.Itoa(rw.status)).Inc()
	}

	mux.HandleFunc(pattern, wrapped)
}

func normalizePattern(pattern string) string {
	p := pathParamRE.ReplaceAllString(pattern, "")
	p = strings.TrimSuffix(p, "/")
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

// PrometheusHandler returns an http.Handler that serves the Prometheus metrics.
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

func (cm *controllerMetrics) DenormWaitingGet() float64 {
	m := &dto.Metric{}
	err := cm.controllerWaiting.WithLabelValues("denorm_waiting").Write(m)
	if err != nil {
		return 0.0
	}
	return m.GetGauge().GetValue()
}

func (cm *controllerMetrics) denormCompletedInc() {
	cm.controllerTotals.WithLabelValues("denorm_completed").Inc()
}

func (cm *controllerMetrics) denormCompletedAdd(delta int64) {
	if delta <= 0 {
		return
	}
	cm.controllerTotals.WithLabelValues("denorm_completed").Add(float64(delta))
}

func (cm *controllerMetrics) DenormCompletedGet() float64 {
	m := &dto.Metric{}
	err := cm.controllerTotals.WithLabelValues("denorm_completed").Write(m)
	if err != nil {
		return 0.0
	}
	return m.GetCounter().GetValue()
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

func (cm *controllerMetrics) RefreshWaitingGet() float64 {
	m := &dto.Metric{}
	err := cm.controllerWaiting.WithLabelValues("refresh_waiting").Write(m)
	if err != nil {
		return 0.0
	}
	return m.GetGauge().GetValue()
}

func (cm *controllerMetrics) refreshCompletedInc() {
	cm.controllerTotals.WithLabelValues("refresh_completed").Inc()
}

func (cm *controllerMetrics) refreshCompletedAdd(delta int64) {
	if delta <= 0 {
		return
	}
	cm.controllerTotals.WithLabelValues("refresh_completed").Add(float64(delta))
}

func (cm *controllerMetrics) RefreshCompletedGet() float64 {
	m := &dto.Metric{}
	err := cm.controllerTotals.WithLabelValues("refresh_completed").Write(m)
	if err != nil {
		return 0.0
	}
	return m.GetCounter().GetValue()
}

func (cm *controllerMetrics) EtagMatchesInc() {
	cm.eTagTotals.WithLabelValues("matches").Inc()
}

func (cm *controllerMetrics) EtagMatchesGet() float64 {
	m := &dto.Metric{}
	err := cm.eTagTotals.WithLabelValues("matches").Write(m)
	if err != nil {
		return 0.0
	}
	return m.GetCounter().GetValue()
}

func (cm *controllerMetrics) EtagMismatchesInc() {
	cm.eTagTotals.WithLabelValues("mismatches").Inc()
}

func (cm *controllerMetrics) EtagMismatchesGet() float64 {
	m := &dto.Metric{}
	err := cm.eTagTotals.WithLabelValues("mismatches").Write(m)
	if err != nil {
		return 0.0
	}
	return m.GetCounter().GetValue()
}

func (cm *controllerMetrics) EtagRatioGet() float64 {
	hits := cm.EtagMatchesGet()
	misses := cm.EtagMismatchesGet()
	if hits+misses == 0 {
		return 0.0
	}
	ratio := hits / (hits + misses)
	return ratio
}

func (cm *noControllerMetrics) DenormWaitingInc()             {}
func (cm *noControllerMetrics) DenormWaitingDec()             {}
func (cm *noControllerMetrics) DenormWaitingAdd(delta int64)  {}
func (cm *noControllerMetrics) DenormWaitingGet() float64     { return 0.0 }
func (cm *noControllerMetrics) RefreshWaitingInc()            {}
func (cm *noControllerMetrics) RefreshWaitingDec()            {}
func (cm *noControllerMetrics) RefreshWaitingAdd(delta int64) {}
func (cm *noControllerMetrics) RefreshWaitingGet() float64    { return 0.0 }
func (cm *noControllerMetrics) EtagMatchesInc()               {}
func (cm *noControllerMetrics) EtagMatchesGet() float64       { return 0.0 }
func (cm *noControllerMetrics) EtagMismatchesInc()            {}
func (cm *noControllerMetrics) EtagMismatchesGet() float64    { return 0.0 }
func (cm *noControllerMetrics) EtagRatioGet() float64         { return 0.0 }

func (cm *cacheMetrics) CacheHitInc() {
	cm.totals.WithLabelValues("hits").Inc()
}

func (cm *cacheMetrics) CacheHitGet() int64 {
	m := &dto.Metric{}
	err := cm.totals.WithLabelValues("hits").Write(m)
	if err != nil {
		return 0.0
	}
	return int64(m.GetCounter().GetValue())
}

func (cm *cacheMetrics) CacheMissInc() {
	cm.totals.WithLabelValues("misses").Inc()
}

func (cm *cacheMetrics) CacheMissGet() int64 {
	m := &dto.Metric{}
	err := cm.totals.WithLabelValues("misses").Write(m)
	if err != nil {
		return 0.0
	}
	return int64(m.GetCounter().GetValue())
}

func (cm *cacheMetrics) CacheHitRatioGet() float64 {
	hits := cm.CacheHitGet()
	misses := cm.CacheMissGet()
	if hits+misses == 0 {
		return 0.0
	}
	ratio := float64(hits) / float64(hits+misses)
	return ratio
}

func (cm *cacheMetrics) CacheBookTotalInc() {
	cm.totals.WithLabelValues("books_total").Inc()
}

func (cm *cacheMetrics) CacheAuthorTotalInc() {
	cm.totals.WithLabelValues("authors_total").Inc()
}

func (cm *cacheMetrics) CacheWorkTotalInc() {
	cm.totals.WithLabelValues("works_total").Inc()
}

func (cm *cacheMetrics) CacheUnknownTotalInc() {
	cm.totals.WithLabelValues("unknown_total").Inc()
}

func (cm *cacheMetrics) CacheZeroTotalInc() {
	cm.totals.WithLabelValues("zero_total").Inc()
}

func (cm *noCacheMetrics) CacheHitInc()              {}
func (cm *noCacheMetrics) CacheHitGet() int64        { return 0 }
func (cm *noCacheMetrics) CacheMissInc()             {}
func (cm *noCacheMetrics) CacheMissGet() int64       { return 0 }
func (cm *noCacheMetrics) CacheHitRatioGet() float64 { return 0.0 }
func (cm *noCacheMetrics) CacheBookTotalInc()        {}
func (cm *noCacheMetrics) CacheAuthorTotalInc()      {}
func (cm *noCacheMetrics) CacheWorkTotalInc()        {}
func (cm *noCacheMetrics) CacheUnknownTotalInc()     {}
func (cm *noCacheMetrics) CacheZeroTotalInc()        {}

func (gm *gqlMetrics) BatchesSentInc() {
	gm.totals.WithLabelValues("batches_sent").Inc()
}

func (gm *gqlMetrics) BatchesSentGet() int64 {
	m := &dto.Metric{}
	err := gm.totals.WithLabelValues("batches_sent").Write(m)
	if err != nil {
		return 0
	}
	return int64(m.GetCounter().GetValue())
}

func (gm *gqlMetrics) QueriesSentInc() {
	gm.totals.WithLabelValues("queries_sent").Inc()
}

func (gm *gqlMetrics) QueriesSentAdd(delta int64) {
	if delta <= 0 {
		return
	}
	gm.totals.WithLabelValues("queries_sent").Add(float64(delta))
}

func (gm *gqlMetrics) QueriesSentGet() int64 {
	m := &dto.Metric{}
	err := gm.totals.WithLabelValues("queries_sent").Write(m)
	if err != nil {
		return 0
	}
	return int64(m.GetCounter().GetValue())
}

func (gm *nogqlMetrics) BatchesSentInc()            {}
func (gm *nogqlMetrics) BatchesSentGet() int64      { return 0 }
func (gm *nogqlMetrics) QueriesSentInc()            {}
func (gm *nogqlMetrics) QueriesSentAdd(delta int64) {}
func (gm *nogqlMetrics) QueriesSentGet() int64      { return 0 }
