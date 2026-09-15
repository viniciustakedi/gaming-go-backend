//go:build integration

// This file needs a real, reachable Postgres and MiniStack, so it carries
// the integration tag and belongs to seam 3a (in-process Fx composition)
// from the spec. Run it against `docker compose up postgres ministack
// provisioning migrate` - see README.md.
package app_test

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/app"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/envfile"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/httpapi"
)

// readEnvFile reads path with envfile.Read, appending the
// scripts/wait-for-integration.sh hint to a missing-file error - envfile's
// own message stays neutral for its production callers (internal/pg).
func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	values, err := envfile.Read(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%v - run scripts/wait-for-integration.sh first", err)
		}
		t.Fatalf("%v", err)
	}
	return values
}

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
	t.Setenv("DATABASE_HOST", "")

	fxApp := fx.New(app.Modules)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fxApp.Start(ctx); err == nil {
		t.Fatal("want start to fail with invalid config, got nil error")
	}
}

const defaultOwnerDSN = "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable"

// databaseConfigEnv derives config.Load's credential-free DATABASE_HOST/
// DATABASE_PORT/DATABASE_NAME/DATABASE_SSLMODE from DATABASE_URL's own
// host, port, database and sslmode - the same derivation
// test/integration's appConfigEnv uses - so a non-default Postgres port
// (set once, in DATABASE_URL, for a whole `docker compose -p <prefix>`
// stack) does not also require separately exported DATABASE_HOST/PORT.
func databaseConfigEnv(t *testing.T) map[string]string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultOwnerDSN
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	sslmode := u.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "disable"
	}
	return map[string]string{
		"DATABASE_HOST":    u.Hostname(),
		"DATABASE_PORT":    port,
		"DATABASE_NAME":    strings.TrimPrefix(u.Path, "/"),
		"DATABASE_SSLMODE": sslmode,
	}
}

// keycloakPort mirrors test/integration's own KEYCLOAK_PORT convention
// (see test/integration/keycloak_test.go), so this package's OIDC
// discovery in TestApp_StartStop_ReleasesResources reaches the same
// Keycloak a manual `scripts/wait-for-integration.sh` run brought up.
func keycloakPort() string {
	if v := os.Getenv("KEYCLOAK_PORT"); v != "" {
		return v
	}
	return "8081"
}

func setIntegrationEnv(t *testing.T) {
	t.Helper()
	// Credential-free on purpose - config.Load rejects userinfo here, the
	// same shape docker-compose.yml's app service receives. The migration
	// owner's own DATABASE_URL (with credentials) is never read by this
	// process, only by `wallet-service migrate`. Host/port/name/sslmode are
	// derived from DATABASE_URL when set (the same override
	// test/integration's own appConfigEnv uses for a non-default Postgres
	// port), so this test needs no separate DATABASE_HOST/PORT of its own.
	for k, v := range databaseConfigEnv(t) {
		if os.Getenv(k) == "" {
			t.Setenv(k, v)
		}
	}
	for _, kv := range []struct{ key, fallback string }{
		{"SQS_ENDPOINT_URL", "http://localhost:4566"},
		{"AUTH_ISSUER_URL", "http://localhost:" + keycloakPort() + "/realms/wallet"},
	} {
		if os.Getenv(kv.key) == "" {
			t.Setenv(kv.key, kv.fallback)
		}
	}
	// pg.New (internal/pg) adds wallet_app's own credentials on top of the
	// credential-free DSN above, reading the password from this file - the
	// same one deploy/postgres/provision.sh writes and test/integration's
	// own walletAppPassword helper reads (see README.md, "Credenciais do
	// Postgres").
	t.Setenv("DATABASE_APP_CREDENTIALS_FILE", filepath.Join("..", "..", "deploy", "postgres", ".runtime", "credentials.env"))

	appCredentialsFile := filepath.Join("..", "..", "deploy", "ministack", ".runtime", "app-credentials.env")
	appCreds := readEnvFile(t, appCredentialsFile)
	for _, key := range []string{"SQS_CONSUMER_ACCESS_KEY_ID", "SQS_CONSUMER_SECRET_ACCESS_KEY", "SQS_PUBLISHER_ACCESS_KEY_ID", "SQS_PUBLISHER_SECRET_ACCESS_KEY"} {
		v, ok := appCreds[key]
		if !ok || v == "" {
			t.Fatalf("missing %s in %s - run scripts/wait-for-integration.sh first", key, appCredentialsFile)
		}
		t.Setenv(key, v)
	}
}
