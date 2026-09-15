package httpapi

import "go.uber.org/fx"

// Module provides the HTTP server and registers its lifecycle. It depends
// on the "readiness" value group, so it must come after every module that
// contributes to it (postgres, sqs) in the composition root - Fx resolves
// this by dependency, not by declaration order, but the fx.Module list in
// internal/app still reads top-to-bottom in startup order for a human.
var Module = fx.Module("http",
	fx.Provide(New),
	fx.Invoke(RegisterLifecycle),
)
