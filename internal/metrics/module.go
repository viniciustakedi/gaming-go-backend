package metrics

import "go.uber.org/fx"

// Module provides the *prometheus.Registry shared by every component that
// records a metric.
var Module = fx.Module("metrics",
	fx.Provide(NewRegistry),
)
