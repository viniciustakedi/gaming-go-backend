package consumer

import "go.uber.org/fx"

var Module = fx.Module("consumer", fx.Provide(New), fx.Invoke(RegisterLifecycle))
