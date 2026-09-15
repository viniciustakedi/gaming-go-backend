package logging

import "go.uber.org/fx"

// Module provides the *slog.Logger used by every other component and
// redirects Fx's own event stream (constructor calls, hook timings) through
// it via fx.WithLogger.
var Module = fx.Module("logging",
	fx.Provide(New),
	fx.WithLogger(FxLogger),
)
