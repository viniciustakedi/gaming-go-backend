// Package httpapi wires the HTTP server: health checks, metrics, and the
// wagering and wallet routes. It owns the process's notion of readiness and
// is the first component to react to shutdown.
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
// without this package importing either - the group is the only coupling.
type ReadinessChecks struct {
	fx.In
	Checks []health.Named `group:"readiness"`
}

// Server bundles the listener and *http.Server so callers can read back the
// actual bound address, which matters when HTTP_ADDR is "127.0.0.1:0".
// Listener is nil until the OnStart hook binds it: New only builds the mux
// and does no I/O - see RegisterLifecycle.
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

func New(cfg config.Config, registry *prometheus.Registry, checks ReadinessChecks, logger *slog.Logger, verifier auth.Verifier, openWallet *walletapp.OpenWalletUseCase, getWallet *walletapp.GetWalletUseCase, ledgerAudit *walletapp.LedgerAuditUseCase, processOperation *walletapp.ProcessOperationUseCase, getTransaction *walletapp.GetTransactionUseCase) (*Server, error) {
	readiness := NewReadiness(checks.Checks, cfg.HTTP.ReadinessTimeout)
	latency := newHTTPLatency(registry)
	reconciliationMetrics := newReconciliationMetrics(registry)

	// Every /wallets* route requires the wallet-admin realm role, and
	// POST /wagering/transactions requires provider, both checked once the
	// mux has matched a route. The two read routes below accept either role:
	// a provider sees only its own transactions and a wallet-admin sees
	// every one, a distinction the route alone cannot express, so
	// GetTransactionUseCase applies it once the caller's role and identity
	// are known. /health/* and /metrics stay unauthenticated.
	providerOrAdmin := []string{auth.RoleProvider, auth.RoleWalletAdmin}
	mux := http.NewServeMux()
	mux.Handle("GET /health/live", liveHandler())
	mux.Handle("GET /health/ready", readyHandler(readiness))
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry}))
	mux.Handle("POST /wallets", latency.wrap("POST /wallets", requireRole(auth.RoleWalletAdmin, openWalletHandler(openWallet, logger))))
	mux.Handle("GET /wallets/{walletId}", latency.wrap("GET /wallets/{walletId}", requireRole(auth.RoleWalletAdmin, getWalletHandler(getWallet, logger))))
	mux.Handle("GET /wallets/{walletId}/ledger", latency.wrap("GET /wallets/{walletId}/ledger", requireRole(auth.RoleWalletAdmin, ledgerHandler(ledgerAudit, logger))))
	mux.Handle("POST /wallets/{walletId}/reconciliation", latency.wrap("POST /wallets/{walletId}/reconciliation", requireRole(auth.RoleWalletAdmin, reconciliationHandler(ledgerAudit, logger, reconciliationMetrics))))
	mux.Handle("POST /wagering/transactions", latency.wrap("POST /wagering/transactions", requireRole(auth.RoleProvider, wageringTransactionsHandler(processOperation, logger))))
	mux.Handle("GET /wagering/transactions/{transactionId}", latency.wrap("GET /wagering/transactions/{transactionId}", requireAnyRole(providerOrAdmin, wageringTransactionByIDHandler(getTransaction, logger))))
	mux.Handle("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", latency.wrap("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", requireAnyRole(providerOrAdmin, providerWageringTransactionHandler(getTransaction, logger))))

	return &Server{
		HTTP: &http.Server{
			// authenticate wraps the whole mux, not just its matched routes,
			// so a business-namespace request the mux itself would answer
			// with a public 404/405 (an unmapped method, a typo'd path)
			// still requires a valid token first.
			Handler:      authenticate(verifier, isPublicRoute, mux),
			ReadTimeout:  cfg.HTTP.ReadTimeout,
			WriteTimeout: cfg.HTTP.WriteTimeout,
		},
		Readiness: readiness,
		addr:      cfg.HTTP.Addr,
	}, nil
}

// isPublicRoute reports whether r's path is exempt from authentication:
// health checks and metrics. Everything else is business namespace and
// default-denied by authenticate until proven public here.
func isPublicRoute(r *http.Request) bool {
	if r.URL.Path == "/metrics" {
		return true
	}
	return strings.HasPrefix(r.URL.Path, "/health/")
}

// RegisterLifecycle binds the listener and serves in a background goroutine
// on start. On stop it fails readiness first, so the shutdown is visible to
// the orchestrator before Shutdown stops accepting new connections and
// drains the ones in flight, bounded by HTTP_SHUTDOWN_TIMEOUT.
//
// The listener is opened here, in OnStart, rather than in New: binding it in
// the constructor would leave the port bound for the rest of the process's
// life whenever a later hook (Postgres ping, SQS resolution) aborts Start()
// before this hook's OnStop was registered as "started".
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
