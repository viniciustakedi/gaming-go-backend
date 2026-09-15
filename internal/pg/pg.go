// Package pg owns the pgx connection pool. It never opens a transaction or
// runs a query itself - that is every repository's job in later tickets -
// it only proves the database is reachable and closes cleanly.
package pg

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
)

// New builds the pool without connecting. pgxpool.New only parses the DSN
// and prepares the pool; the OnStart hook registered below is what proves
// connectivity, with a bounded timeout, before the app is allowed to serve
// traffic.
func New(cfg config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Postgres.DSN)
	if err != nil {
		return nil, fmt.Errorf("pg: parse DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = cfg.Postgres.MaxConns

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("pg: create pool: %w", err)
	}
	return pool, nil
}

// RegisterLifecycle pings Postgres on start, within the configured timeout,
// and closes the pool on stop. Fx runs OnStop hooks in the reverse order
// their OnStart counterpart was registered: because the pool is a
// constructor dependency of the HTTP server and every future consumer, its
// hook is registered before theirs, so on shutdown it closes only after they
// have already stopped using it.
func RegisterLifecycle(lc fx.Lifecycle, pool *pgxpool.Pool, cfg config.Config, logger *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, cfg.Postgres.PingTimeout)
			defer cancel()
			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("pg: ping failed, database unreachable: %w", err)
			}
			logger.Info("postgres reachable")
			return nil
		},
		OnStop: func(ctx context.Context) error {
			pool.Close()
			logger.Info("postgres pool closed")
			return nil
		},
	})
}

// Ping is the readiness probe used by internal/httpapi. It is a thin,
// short-timeout wrapper so the HTTP handler never blocks on a stalled
// database for longer than the caller allows.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	return pool.Ping(ctx)
}
