package referenceworker

import "go.uber.org/fx"

var Module = fx.Module("referenceworker", fx.Provide(New), fx.Invoke(RegisterLifecycle))
