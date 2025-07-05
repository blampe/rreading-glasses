package internal

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func init() {
	reg := prometheus.NewRegistry()

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

var (
	// strip all `{...}` segments from the pattern to build a label
	pathParamRE = regexp.MustCompile(`\{[^/]+\}`)
)

type RequestPromMiddleware struct {
	hist *prometheus.HistogramVec
}

func NewRequestPromMiddleware() *RequestPromMiddleware {
	hist := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "myapp",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request latencies by method & path",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"method", "path", "status"},
	)
	prometheus.MustRegister(hist)
	return &RequestPromMiddleware{hist: hist}
}

func (m *RequestPromMiddleware) HandleFunc(
	mux *http.ServeMux,
	pattern string,
	hf http.HandlerFunc,
) {
	// derive the constant label from the pattern:
	//   "/author/{foreignAuthorID}" → "/author"
	//   "/book/bulk"                → "/book/bulk"
	label := normalizePattern(pattern)

	wrapped := func(w http.ResponseWriter, r *http.Request) {
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		hf(rw, r)

		dur := time.Since(start).Seconds()
		m.hist.
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

func PrometheusHandler() http.Handler {
	return promhttp.Handler()
}
