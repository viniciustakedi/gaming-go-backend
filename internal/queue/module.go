package queue

import "go.uber.org/fx"

// Module provides the role-scoped SQS clients, resolves the queue URLs on
// start and contributes a readiness Check to the "readiness" value group.
var Module = fx.Module("sqs",
	fx.Provide(New),
	fx.Provide(NewOutboxPublisher),
	fx.Provide(
		fx.Annotate(
			readinessCheck,
			fx.ResultTags(`group:"readiness"`),
		),
	),
	fx.Invoke(RegisterLifecycle),
)
