package httpapi

import "go.uber.org/fx"

// Module provides the HTTP server and registers its lifecycle. It depends
// on the "readiness" value group; Fx resolves that by dependency, not by
// declaration order, but internal/app still lists it after postgres and sqs
// so the list reads top-to-bottom in startup order for a human.
var Module = fx.Module("http",
	fx.Provide(New),
	fx.Invoke(RegisterLifecycle),
)
