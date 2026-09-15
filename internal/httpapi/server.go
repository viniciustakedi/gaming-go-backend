// Package httpapi wires the HTTP server: health checks, metrics and, from
// later tickets, the wagering and wallet routes. It owns the process's
// notion of readiness and is the first component to react to shutdown.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/auth"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/health"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

// ReadinessChecks is an fx.In struct that collects every Check contributed
// to the "readiness" value group. Postgres and SQS each add their own
// (internal/pg, internal/queue) without this package importing either -
// the group is the only coupling between them.
type ReadinessChecks struct {
	fx.In
	Checks []health.Named `group:"readiness"`
}

// Server bundles the listener and *http.Server so callers - notably the
// fxtest composition test - can read back the actual bound address, which
// matters when HTTP_ADDR is "127.0.0.1:0". Listener is nil until the
// OnStart hook below binds it: New only builds the mux and does no I/O, so a
// later hook failing (Postgres, SQS) during the same Start() call means this
// hook never runs and the port is never touched at all - see RegisterLifecycle.
type Server struct {
	Listener  net.Listener
	HTTP      *http.Server
	Readiness *Readiness

	addr string
}

// Addr returns the address the server is actually listening on. Only valid
// after the OnStart hook has run.
func (s *Server) Addr() string {
	return s.Listener.Addr().String()
}

func New(cfg config.Config, registry *prometheus.Registry, checks ReadinessChecks, logger *slog.Logger, verifier auth.Verifier, openWallet *walletapp.OpenWalletUseCase, getWallet *walletapp.GetWalletUseCase, processOperation *walletapp.ProcessOperationUseCase) (*Server, error) {
	readiness := NewReadiness(checks.Checks, cfg.HTTP.ReadinessTimeout)
	latency := newHTTPLatency(registry)

	// Every /wallets* route requires the wallet-admin realm role, and
	// /wagering/transactions requires provider, both checked by requireRole
	// once the mux has matched a route. /health/* and /metrics stay
	// unauthenticated (spec, decision 7).
	mux := http.NewServeMux()
	mux.Handle("GET /health/live", liveHandler())
	mux.Handle("GET /health/ready", readyHandler(readiness))
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry}))
	mux.Handle("POST /wallets", latency.wrap("POST /wallets", requireRole(auth.RoleWalletAdmin, openWalletHandler(openWallet, logger))))
	mux.Handle("GET /wallets/{walletId}", latency.wrap("GET /wallets/{walletId}", requireRole(auth.RoleWalletAdmin, getWalletHandler(getWallet, logger))))
	mux.Handle("POST /wagering/transactions", latency.wrap("POST /wagering/transactions", requireRole(auth.RoleProvider, wageringTransactionsHandler(processOperation, logger))))

	return &Server{
		HTTP: &http.Server{
			// authenticate wraps the whole mux, not just its matched routes,
			// so a business-namespace request the mux itself would answer
			// with a public 404/405 (an unmapped method, a typo'd path)
			// still requires a valid token first (ticket 07 review: "PUT
			// /wallets sem token recebe o 405 público do mux, em vez de
			// 401").
			Handler:      authenticate(verifier, isPublicRoute, mux),
			ReadTimeout:  cfg.HTTP.ReadTimeout,
			WriteTimeout: cfg.HTTP.WriteTimeout,
		},
		Readiness: readiness,
		addr:      cfg.HTTP.Addr,
	}, nil
}

// isPublicRoute reports whether r's path is exempt from authentication:
// health checks and metrics (spec, decision 7: "/health/* é público. /metrics
// é público"). Everything else - today just /wallets*, and later /wagering*
// and /providers* without any change to this function (ticket 07: "o
// desenho deve acomodar /wagering e /providers sem mudança estrutural") - is
// business namespace and default-denied by authenticate until proven public
// here.
func isPublicRoute(r *http.Request) bool {
	if r.URL.Path == "/metrics" {
		return true
	}
	return strings.HasPrefix(r.URL.Path, "/health/")
}

// RegisterLifecycle binds the listener and starts serving in a background
// goroutine on start, and, on stop, enforces the exact ordering the spec
// requires: readiness fails first, so the shutdown is visible to the
// orchestrator immediately, and only then does Shutdown stop accepting new
// connections and drain the ones already in flight, bounded by
// HTTP_SHUTDOWN_TIMEOUT.
//
// The listener is opened here, in OnStart, rather than in New. If it were
// opened in the constructor, it would be bound as soon as the Fx object
// graph is built - before any lifecycle hook runs - so a later hook failing
// (Postgres ping, SQS resolution) would abort Start() without this hook's
// OnStop ever having been registered as "started", leaving the port bound
// for the rest of the process's life. Binding it here means the port is
// only ever touched once this hook's own turn to start has come, so an
// earlier hook's failure leaves it untouched and free for a retry.
func RegisterLifecycle(lc fx.Lifecycle, s *Server, cfg config.Config, logger *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			listener, err := net.Listen("tcp", s.addr)
			if err != nil {
				return fmt.Errorf("httpapi: listen on %s: %w", s.addr, err)
			}
			s.Listener = listener

			go func() {
				if err := s.HTTP.Serve(s.Listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("http server stopped unexpectedly", "error", err)
				}
			}()
			logger.Info("http server listening", "addr", s.Addr())
			return nil
		},
		OnStop: func(ctx context.Context) error {
			s.Readiness.MarkNotReady()

			shutdownCtx, cancel := context.WithTimeout(ctx, cfg.HTTP.ShutdownTimeout)
			defer cancel()
			if err := s.HTTP.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("httpapi: shutdown: %w", err)
			}
			logger.Info("http server stopped")
			return nil
		},
	})
}
