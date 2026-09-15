package config

import "go.uber.org/fx"

// Module provides the validated Config to the Fx graph. Load runs inside the
// constructor, so an invalid environment fails fx.New itself - no component
// downstream ever observes a half-valid Config.
var Module = fx.Module("config",
	fx.Provide(Load),
)
