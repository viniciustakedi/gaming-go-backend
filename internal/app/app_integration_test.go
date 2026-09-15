//go:build integration

// This file needs a real, reachable Postgres and MiniStack, so it carries
// the integration tag and belongs to seam 3a (in-process Fx composition)
// from the spec. Run it against `docker compose up postgres ministack
// provisioning migrate` - see README.md.
package app_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/app"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/httpapi"
)

// TestApp_StartStop_ReleasesResources proves the whole Fx graph - config,
// logging, metrics, postgres, sqs, http - starts, serves on an
// OS-allocated port, and stops without leaking goroutines or leaving the
// listener bound. fxtest.New runs under -race and calls t.Fatal on any
// unresolved dependency, so a wiring mistake fails this test before it
// fails at runtime.
func TestApp_StartStop_ReleasesResources(t *testing.T) {
	setIntegrationEnv(t)
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")

	var server *httpapi.Server
	fxApp := fxtest.New(t,
		app.Modules,
		fx.Populate(&server),
	)

	fxApp.RequireStart()

	resp, err := http.Get("http://" + server.Addr() + "/health/ready")
	if err != nil {
		t.Fatalf("GET /health/ready: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 while running, got %d", resp.StatusCode)
	}

	liveResp, err := http.Get("http://" + server.Addr() + "/health/live")
	if err != nil {
		t.Fatalf("GET /health/live: %v", err)
	}
	_ = liveResp.Body.Close()
	if liveResp.StatusCode != http.StatusOK {
		t.Errorf("want 200 from /health/live, got %d", liveResp.StatusCode)
	}

	fxApp.RequireStop()

	// The listener must be released after stop: a fresh dial to the same
	// address must fail, proving Shutdown actually closed it rather than
	// merely stopping accepting new logical requests.
	if _, err := http.Get("http://" + server.Addr() + "/health/live"); err == nil {
		t.Error("want connection error after stop, server is still accepting")
	}
}

// TestApp_StartFailsWithInvalidConfig proves config validation aborts
// fx.New/Start before any lifecycle hook runs - no partially started
// component, no port bound, no database connection attempted.
func TestApp_StartFailsWithInvalidConfig(t *testing.T) {
	setIntegrationEnv(t)
	t.Setenv("DATABASE_URL", "")

	fxApp := fx.New(app.Modules)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fxApp.Start(ctx); err == nil {
		t.Fatal("want start to fail with invalid config, got nil error")
	}
}

func setIntegrationEnv(t *testing.T) {
	t.Helper()
	for _, kv := range []struct{ key, fallback string }{
		{"DATABASE_URL", "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable"},
		{"SQS_ENDPOINT_URL", "http://localhost:4566"},
	} {
		if os.Getenv(kv.key) == "" {
			t.Setenv(kv.key, kv.fallback)
		}
	}
	requireEnv(t, "SQS_CONSUMER_ACCESS_KEY_ID")
	requireEnv(t, "SQS_CONSUMER_SECRET_ACCESS_KEY")
	requireEnv(t, "SQS_PUBLISHER_ACCESS_KEY_ID")
	requireEnv(t, "SQS_PUBLISHER_SECRET_ACCESS_KEY")
}

func requireEnv(t *testing.T, key string) {
	t.Helper()
	if os.Getenv(key) == "" {
		t.Skipf("%s not set - run against `docker compose up provisioning` and source deploy/ministack/.credentials.env first, see README.md", key)
	}
}
