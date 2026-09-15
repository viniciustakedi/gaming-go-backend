// Package metrics owns the Prometheus registry. Other modules register
// their own collectors into it through fx.Provide constructors that accept
// *prometheus.Registry - this package never imports them, so it stays
// ignorant of what the rest of the service measures.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// NewRegistry returns a dedicated registry instead of the global
// prometheus.DefaultRegisterer. A dedicated registry keeps /metrics free of
// whatever a transitively imported package might register on the default
// one, and makes the exposed set of metrics an explicit, reviewable list.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}
