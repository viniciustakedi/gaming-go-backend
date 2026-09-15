package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// httpLatency records how long each handled request took, labeled by route
// and outcome - the "histograma de latência de processamento" the spec asks
// for under Observability. The route label is the literal mux pattern
// ("POST /wallets"), not the raw request path, so a thousand different
// wallet ids are one time series, not one per wallet.
type httpLatency struct {
	duration *prometheus.HistogramVec
}

func newHTTPLatency(registry *prometheus.Registry) *httpLatency {
	m := &httpLatency{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by route and status.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "status"}),
	}
	registry.MustRegister(m.duration)
	return m
}

// wrap times next and records it under route, labeled by the status code
// next actually wrote.
func (m *httpLatency) wrap(route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next(recorder, r)
		m.duration.WithLabelValues(route, strconv.Itoa(recorder.status)).Observe(time.Since(start).Seconds())
	}
}

// statusRecorder captures the status code a handler writes, since
// http.ResponseWriter has no getter of its own.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
