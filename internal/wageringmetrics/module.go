package wageringmetrics

import "go.uber.org/fx"

// Module provides the walletapp.OperationMetrics port, backed by the
// application's shared Prometheus registry.
var Module = fx.Module("wageringmetrics",
	fx.Provide(New),
)
