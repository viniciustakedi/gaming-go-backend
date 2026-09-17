// Package wageringmetrics is the Prometheus-backed adapter for
// walletapp.OperationMetrics - the one place that turns the shared
// processing use case's observability signals into actual collectors,
// keeping internal/walletapp itself free of a concrete metrics library.
package wageringmetrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

type metrics struct {
	operations *prometheus.CounterVec
	duplicates *prometheus.CounterVec
	conflicts  prometheus.Counter
	latency    *prometheus.HistogramVec
}

// New registers and returns the wagering collectors on registry.
func New(registry *prometheus.Registry) walletapp.OperationMetrics {
	m := &metrics{
		operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wagering_operations_total",
			Help: "Wager operations processed, by channel, kind and resulting status.",
		}, []string{"channel", "kind", "status"}),
		duplicates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wagering_duplicate_attempts_total",
			Help: "Idempotent replays observed, by channel.",
		}, []string{"channel"}),
		conflicts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wagering_concurrency_conflicts_total",
			Help: "Concurrency conflicts detected at the wager-transaction insert backstop or the wallet version check.",
		}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wagering_processing_duration_seconds",
			Help:    "Wager operation processing latency in seconds, by channel and kind.",
			Buckets: prometheus.DefBuckets,
		}, []string{"channel", "kind"}),
	}
	registry.MustRegister(m.operations, m.duplicates, m.conflicts, m.latency)
	return m
}

func (m *metrics) ObserveOperation(channel, kind, status string, duration time.Duration) {
	m.operations.WithLabelValues(channel, kind, status).Inc()
	m.latency.WithLabelValues(channel, kind).Observe(duration.Seconds())
}

func (m *metrics) ObserveDuplicate(channel string) {
	m.duplicates.WithLabelValues(channel).Inc()
}

func (m *metrics) ObserveConcurrencyConflict() {
	m.conflicts.Inc()
}
