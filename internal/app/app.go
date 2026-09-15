// Package app wires every module into a single Fx application. It is the
// only place cmd/wallet-service and the fxtest composition test need to
// import, so the two never drift apart.
package app

import (
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/auth"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/httpapi"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/logging"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/metrics"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/outbox"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/outboxpg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/pg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/queue"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/wageringmetrics"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletpg"
)

// Modules lists every Fx module the service composes, in the order a human
// reads as "config and logging first, dependencies next, the server last".
// The actual start order is decided by Fx from the dependency graph, not
// from this slice's order.
var Modules = fx.Options(
	config.Module,
	logging.Module,
	metrics.Module,
	pg.Module,
	queue.Module,
	auth.Module,
	wageringmetrics.Module,
	walletpg.Module,
	outboxpg.Module,
	outbox.Module,
	httpapi.Module,
)

// New builds the fx.App. extra lets callers (notably fxtest) append
// fx.Populate, fx.Replace or fx.Decorate options without this package
// needing to know about them.
func New(extra ...fx.Option) *fx.App {
	opts := append([]fx.Option{
		Modules,
		fx.StopTimeout(config.StopTimeoutFromEnv()),
	}, extra...)
	return fx.New(opts...)
}
