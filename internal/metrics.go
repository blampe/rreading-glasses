package internal

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/IBM/pgxpoolprometheus"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	dto "github.com/prometheus/client_model/go"
)

// NewMetrics creates a new Rrometheus registry with default collectors already
// registered.
func NewMetrics() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{
			Namespace: _metricsNamespace,
		}),
		collectors.NewBuildInfoCollector(),
	)

	return reg
}

var _metricsNamespace = "rg"

// _patternRE is used for stripping all `{...}` segments from the pattern
// to build a label.
var _patternRE = regexp.MustCompile(`\{[^/]+\}`)

type controllerMetrics struct {
	totals *prometheus.CounterVec
	gauge  *prometheus.GaugeVec
}

type cacheMetrics struct {
	totals *prometheus.CounterVec
}

type gqlMetrics struct {
	totals *prometheus.CounterVec
}

type dbMetrics struct {
	dirty atomic.Bool // dirty signals that the DB has been modified so stats should be collected.
	gauge *prometheus.GaugeVec
}

// instrument wraps an HTTP handler to automatically record timing and status
// codes.
func instrument(reg *prometheus.Registry, next http.Handler) http.Handler {
	requests := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: _metricsNamespace,
			Subsystem: "http",
			Name:      "requests",
			Help:      "HTTP request latencies by method & path",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 1.5, 2.0, 2.5, 5, 7.5, 10, 30, 60, 120},
		},
		[]string{"method", "path", "status"},
	)

	inflight := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: _metricsNamespace,
			Subsystem: "http",
			Name:      "inflight",
			Help:      "Current number of inbound in-flight HTTP requests.",
		},
	)

	normalized := map[string]string{}

	reg.MustRegister(requests, inflight)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		inflight.Inc()
		defer inflight.Dec()

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		path, ok := normalized[r.Pattern]
		if !ok {
			path = normalizePattern(r.Pattern)
			normalized[r.Pattern] = path
		}
		if path == "" {
			// Don't record traffic for unrecognized endpoints.
			return
		}

		duration := time.Since(start).Seconds()
		requests.WithLabelValues(r.Method, path, fmt.Sprint(ww.Status())).Observe(duration)
	})
}

func newControllerMetrics(reg *prometheus.Registry) *controllerMetrics {
	totals := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: _metricsNamespace,
			Subsystem: "controller",
			Name:      "total_operations",
			Help:      "Counts of controller operations by type.",
		},
		[]string{"type"},
	)
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: _metricsNamespace,
			Subsystem: "controller",
			Name:      "pending_operations",
			Help:      "Counts of pending controller operations by type.",
		},
		[]string{"type"},
	)
	if reg != nil {
		reg.MustRegister(totals, gauge)
	}
	return &controllerMetrics{
		totals: totals,
		gauge:  gauge,
	}
}

func newCacheMetrics(reg *prometheus.Registry) *cacheMetrics {
	totals := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: _metricsNamespace,
			Subsystem: "cache",
			Name:      "total",
			Help:      "Totals for cache system.",
		},
		[]string{"type"},
	)
	if reg != nil {
		reg.MustRegister(totals)
	}
	return &cacheMetrics{totals: totals}
}

func newGQLMetrics(reg *prometheus.Registry) *gqlMetrics {
	totals := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: _metricsNamespace,
			Subsystem: "gql",
			Name:      "total",
			Help:      "How many batches have been sent.",
		},
		[]string{"type"},
	)
	if reg != nil {
		reg.MustRegister(totals)
	}
	return &gqlMetrics{totals: totals}
}

func newDBMetrics(db *pgxpool.Pool, reg *prometheus.Registry) *dbMetrics {
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: _metricsNamespace,
			Subsystem: "db",
			Name:      "total",
			Help:      "Counts of persisted objects by type.",
		},
		[]string{"type"},
	)
	if reg != nil {
		reg.MustRegister(gauge, pgxpoolprometheus.NewCollector(db, nil))
	}
	dbm := &dbMetrics{gauge: gauge}
	// This is an expensive query so we only run it every 5 minutes,
	// and only if there's been some DB activity that changed the
	// relevant stats.
	dbm.dirty.Store(true) // Start dirty to trigger an initial query.
	go func() {
		ctx := context.Background()
		for {
			row := db.QueryRow(ctx, `
			  SELECT
				sum(CASE WHEN key LIKE 'a%'  THEN 1 ELSE 0 END) AS authors,
				sum(CASE WHEN key LIKE 'b%'  THEN 1 ELSE 0 END) AS editions,
				sum(CASE WHEN key LIKE 'w%'  THEN 1 ELSE 0 END) AS works,
				sum(CASE WHEN key LIKE 'ra%' THEN 1 ELSE 0 END) AS refreshing,
				sum(CASE WHEN key LIKE 's%'  THEN 1 ELSE 0 END) AS seriess,
				sum(CASE WHEN key LIKE 'z%'  THEN 1 ELSE 0 END) AS asin
			  FROM cache;
			`)
			var authors, editions, works, refreshing, series, asin int64
			err := row.Scan(&authors, &editions, &works, &refreshing, &series, &asin)
			if err != nil {
				Log(ctx).Warn("problem collecting db stats", "err", err)
			} else {
				dbm.authorsSet(authors)
				dbm.editionsSet(editions)
				dbm.worksSet(works)
				dbm.refreshingSet(refreshing)
				dbm.seriesSet(series)
				dbm.asinSet(asin)
			}
			dbm.dirty.Store(false)
			time.Sleep(5 * time.Minute)
		}
	}()
	return &dbMetrics{gauge: gauge}
}

func (dbm *dbMetrics) authorsSet(n int64) {
	dbm.gauge.WithLabelValues("authors").Set(float64(n))
}

func (dbm *dbMetrics) editionsSet(n int64) {
	dbm.gauge.WithLabelValues("editions").Set(float64(n))
}

func (dbm *dbMetrics) worksSet(n int64) {
	dbm.gauge.WithLabelValues("works").Set(float64(n))
}

func (dbm *dbMetrics) refreshingSet(n int64) {
	dbm.gauge.WithLabelValues("refreshing").Set(float64(n))
}

func (dbm *dbMetrics) asinSet(n int64) {
	dbm.gauge.WithLabelValues("asins").Set(float64(n))
}

func (dbm *dbMetrics) seriesSet(n int64) {
	dbm.gauge.WithLabelValues("series").Set(float64(n))
}

func (cm *controllerMetrics) denormWaitingAdd(delta int64) {
	if delta == 0 {
		return
	}
	cm.gauge.WithLabelValues("denormalization").Add(float64(delta))
}

func (cm *controllerMetrics) denormWaitingGet() float64 {
	m := &dto.Metric{}
	err := cm.gauge.WithLabelValues("denormalization").Write(m)
	if err != nil {
		return 0.0
	}
	return m.GetGauge().GetValue()
}

func (cm *controllerMetrics) refreshWaitingAdd(delta int64) {
	if delta == 0 {
		return
	}
	cm.gauge.WithLabelValues("refresh").Add(float64(delta))
}

func (cm *controllerMetrics) refreshWaitingGet() float64 {
	m := &dto.Metric{}
	err := cm.gauge.WithLabelValues("refresh").Write(m)
	if err != nil {
		return 0.0
	}
	return m.GetGauge().GetValue()
}

func (cm *controllerMetrics) etagMatchesInc() {
	cm.totals.WithLabelValues("etag_matches").Inc()
}

func (cm *controllerMetrics) etagMatchesGet() float64 {
	m := &dto.Metric{}
	err := cm.totals.WithLabelValues("etag_matches").Write(m)
	if err != nil {
		return 0.0
	}
	return m.GetCounter().GetValue()
}

func (cm *controllerMetrics) etagMismatchesInc() {
	cm.totals.WithLabelValues("etag_mismatches").Inc()
}

func (cm *controllerMetrics) etagMismatchesGet() float64 {
	m := &dto.Metric{}
	err := cm.totals.WithLabelValues("etag_mismatches").Write(m)
	if err != nil {
		return 0.0
	}
	return m.GetCounter().GetValue()
}

func (cm *controllerMetrics) etagRatioGet() float64 {
	hits := cm.etagMatchesGet()
	misses := cm.etagMismatchesGet()
	if hits+misses == 0 {
		return 0.0
	}
	ratio := hits / (hits + misses)
	return ratio
}

func (cm *cacheMetrics) cacheHitInc() {
	cm.totals.WithLabelValues("hits").Inc()
}

func (cm *cacheMetrics) cacheHitGet() int64 {
	m := &dto.Metric{}
	err := cm.totals.WithLabelValues("hits").Write(m)
	if err != nil {
		return 0.0
	}
	return int64(m.GetCounter().GetValue())
}

func (cm *cacheMetrics) cacheMissInc() {
	cm.totals.WithLabelValues("misses").Inc()
}

func (cm *cacheMetrics) cacheMissGet() int64 {
	m := &dto.Metric{}
	err := cm.totals.WithLabelValues("misses").Write(m)
	if err != nil {
		return 0.0
	}
	return int64(m.GetCounter().GetValue())
}

func (cm *cacheMetrics) cacheHitRatioGet() float64 {
	hits := cm.cacheHitGet()
	misses := cm.cacheMissGet()
	if hits+misses == 0 {
		return 0.0
	}
	ratio := float64(hits) / float64(hits+misses)
	return ratio
}

func (gm *gqlMetrics) batchesSentInc() {
	gm.totals.WithLabelValues("batches_sent").Inc()
}

func (gm *gqlMetrics) batchesSentGet() int64 {
	m := &dto.Metric{}
	err := gm.totals.WithLabelValues("batches_sent").Write(m)
	if err != nil {
		return 0
	}
	return int64(m.GetCounter().GetValue())
}

func (gm *gqlMetrics) queriesSentAdd(delta int64) {
	if delta <= 0 {
		return
	}
	gm.totals.WithLabelValues("queries_sent").Add(float64(delta))
}

func (gm *gqlMetrics) queriesSentGet() int64 {
	m := &dto.Metric{}
	err := gm.totals.WithLabelValues("queries_sent").Write(m)
	if err != nil {
		return 0
	}
	return int64(m.GetCounter().GetValue())
}

// normalizePattern derives the constant label from the pattern:
//
//	"/author/{foreignAuthorID}" → "/author"
//	"/book/bulk"                → "/book/bulk"
func normalizePattern(pattern string) string {
	p := _patternRE.ReplaceAllString(pattern, "")
	p = strings.TrimSuffix(p, "/")
	p = strings.ReplaceAll(p, "//", "/")
	return p
}
