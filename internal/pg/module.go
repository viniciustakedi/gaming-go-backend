package pg

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/health"
)

func readinessCheck(pool *pgxpool.Pool) health.Named {
	return health.Named{
		Name: "postgres",
		Check: func(ctx context.Context) error {
			return Ping(ctx, pool)
		},
	}
}

// Module provides the *pgxpool.Pool, registers its start/stop lifecycle and
// contributes a readiness Check to the "readiness" value group that
// internal/httpapi fans out over on every /health/ready request.
var Module = fx.Module("postgres",
	fx.Provide(New),
	fx.Provide(
		fx.Annotate(
			readinessCheck,
			fx.ResultTags(`group:"readiness"`),
		),
	),
	fx.Invoke(RegisterLifecycle),
)
