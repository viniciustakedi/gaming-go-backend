package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/auth"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
)

func testHTTPConfig(addr string) config.Config {
	return config.Config{
		HTTP: config.HTTPConfig{
			Addr:             addr,
			ReadTimeout:      time.Second,
			WriteTimeout:     time.Second,
			ShutdownTimeout:  time.Second,
			ReadinessTimeout: time.Second,
		},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// freeAddr finds a currently unused loopback address by binding to port 0
// and releasing it immediately, so tests can reuse it deterministically.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release probe listener: %v", err)
	}
	return addr
}

// TestRegisterLifecycle_EarlierHookFailureLeavesPortFree simulates a
// Postgres or SQS lifecycle hook failing before httpapi's own hook runs
// during the same Start() call. fx.Hook's contract guarantees a hook whose
// OnStart never ran also never gets its OnStop called, so if New bound the
// listener eagerly, nothing would ever release it. RegisterLifecycle binds
// the listener only inside its own OnStart, so when it never gets to run,
// the port is never touched and stays free for a subsequent attempt.
func TestRegisterLifecycle_EarlierHookFailureLeavesPortFree(t *testing.T) {
	addr := freeAddr(t)
	cfg := testHTTPConfig(addr)
	logger := discardLogger()

	server, err := New(cfg, prometheus.NewRegistry(), ReadinessChecks{}, logger, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	lc := fxtest.NewLifecycle(t)
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			return errors.New("simulated earlier dependency failure (postgres/sqs)")
		},
	})
	RegisterLifecycle(lc, server, cfg, logger)

	if err := lc.Start(context.Background()); err == nil {
		t.Fatal("want the earlier hook's failure to abort Start, got nil error")
	}

	probe, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("want %s free after a failed start, got: %v", addr, err)
	}
	_ = probe.Close()
}

// TestRegisterLifecycle_StopReleasesPortForRetry proves the ordinary
// success path also releases the port on stop, so a start/stop/start cycle
// on the very same address works - the scenario a process restart after a
// clean shutdown relies on.
//
// It never picks a port by binding a throwaway listener and closing it
// (that leaves a window, between the close and the real bind, for anything
// else on the machine to grab the same port): attempt 1 binds
// "127.0.0.1:0" itself, so the OS hands out a port that is free at the
// exact instant it's claimed, and only the address this run's own Listener
// actually got back (server.Addr()) is reused for attempt 2.
//
// It also does not call Stop right after Start. OnStart hands the listener
// to (*http.Server).Serve on a goroutine and returns immediately - Serve
// only registers that listener with the server, so Shutdown knows to close
// it, once the goroutine actually runs, which OnStart never waits for. Stop
// immediately after Start races that registration: net/http's Shutdown
// finds no registered listener yet, returns success without closing
// anything, and the port stays bound until the goroutine's own deferred
// close eventually runs - which can lose to the next attempt's Listen.
// A real round trip against the freshly started server is a genuine
// synchronization point that closes that race without touching production
// code: Serve registers its listener before it can Accept a connection, so
// a successful response proves the registration already happened, and every
// real caller of this server only ever stops it after it has been serving
// traffic - never microseconds after Start with no request in between.
func TestRegisterLifecycle_StopReleasesPortForRetry(t *testing.T) {
	logger := discardLogger()
	addr := "127.0.0.1:0"

	for attempt := 1; attempt <= 2; attempt++ {
		cfg := testHTTPConfig(addr)
		server, err := New(cfg, prometheus.NewRegistry(), ReadinessChecks{}, logger, nil, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("attempt %d: New: %v", attempt, err)
		}

		lc := fxtest.NewLifecycle(t)
		RegisterLifecycle(lc, server, cfg, logger)

		if err := lc.Start(context.Background()); err != nil {
			t.Fatalf("attempt %d: Start: %v", attempt, err)
		}
		addr = server.Addr()

		resp, err := http.Get("http://" + addr + "/health/live")
		if err != nil {
			t.Fatalf("attempt %d: health check: %v", attempt, err)
		}
		_ = resp.Body.Close()

		if err := lc.Stop(context.Background()); err != nil {
			t.Fatalf("attempt %d: Stop: %v", attempt, err)
		}
	}
}

// TestServer_BusinessNamespace_AuthenticatesBeforeRouting proves ticket 07
// review's fix directly: a method New's mux never registers for /wallets or
// /wallets/{walletId} (PUT, DELETE, PATCH, HEAD, OPTIONS) must answer 401
// when unauthenticated - not the mux's own public 405 - and, once a valid
// token is presented, fall through to the mux's ordinary 405 for a method
// no route maps. This exercises Server.HTTP.Handler exactly as
// RegisterLifecycle serves it, so it also proves authenticate really does
// wrap the whole mux, not just the two registered routes.
func TestServer_BusinessNamespace_AuthenticatesBeforeRouting(t *testing.T) {
	verifier := fakeVerifier{identity: auth.Identity{Subject: "wallet-service-sub", Roles: []string{auth.RoleWalletAdmin}}}
	cfg := testHTTPConfig(freeAddr(t))
	server, err := New(cfg, prometheus.NewRegistry(), ReadinessChecks{}, discardLogger(), verifier, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		method     string
		path       string
		token      bool
		wantStatus int
	}{
		{http.MethodPut, "/wallets", false, http.StatusUnauthorized},
		{http.MethodPut, "/wallets", true, http.StatusMethodNotAllowed},
		{http.MethodDelete, "/wallets", false, http.StatusUnauthorized},
		{http.MethodDelete, "/wallets", true, http.StatusMethodNotAllowed},
		{http.MethodPatch, "/wallets", false, http.StatusUnauthorized},
		{http.MethodPatch, "/wallets", true, http.StatusMethodNotAllowed},
		{http.MethodPut, "/wallets/w-1", false, http.StatusUnauthorized},
		{http.MethodPut, "/wallets/w-1", true, http.StatusMethodNotAllowed},
		{http.MethodDelete, "/wallets/w-1", false, http.StatusUnauthorized},
		{http.MethodDelete, "/wallets/w-1", true, http.StatusMethodNotAllowed},
		{http.MethodPatch, "/wallets/w-1", false, http.StatusUnauthorized},
		{http.MethodPatch, "/wallets/w-1", true, http.StatusMethodNotAllowed},
		{http.MethodHead, "/wallets", false, http.StatusUnauthorized},
		{http.MethodOptions, "/wallets", false, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		name := tc.method + " " + tc.path
		if tc.token {
			name += " (with token)"
		} else {
			name += " (no token)"
		}
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.token {
				req.Header.Set("Authorization", "Bearer whatever")
			}
			rec := httptest.NewRecorder()
			server.HTTP.Handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d, body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestServer_WalletsRoutes_ProviderWithWalletAdminNoProviderID_Preserves403
// proves the re-review fix (Major #1): the wallet-admin-wins precedence
// requireAnyRole added for the two read routes must not reach /wallets* or
// POST /wagering/transactions, which keep requireRole - the exact same
// implementation as before ticket 09 touched auth_middleware.go. On main,
// a token carrying both provider and wallet-admin roles but no provider_id
// is rejected as forbidden by requireRole's own global provider_id check,
// before the wallet-admin role is even considered; that must still hold
// here.
func TestServer_WalletsRoutes_ProviderWithWalletAdminNoProviderID_Preserves403(t *testing.T) {
	verifier := fakeVerifier{identity: auth.Identity{
		Subject: "sub",
		Roles:   []string{auth.RoleProvider, auth.RoleWalletAdmin},
	}}
	cfg := testHTTPConfig(freeAddr(t))
	server, err := New(cfg, prometheus.NewRegistry(), ReadinessChecks{}, discardLogger(), verifier, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/wallets"},
		{http.MethodGet, "/wallets/w-1"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer whatever")
			rec := httptest.NewRecorder()
			server.HTTP.Handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (same as main), body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}
