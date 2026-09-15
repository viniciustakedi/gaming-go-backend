//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/app"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/httpapi"
)

// appHarness is the reusable seam 3a fixture the spec asks ticket 06 to
// establish: the whole Fx application, started via fxtest on an
// OS-assigned port against the real Postgres and MiniStack this package
// already talks to for seam 2. Every later ticket that adds an HTTP
// endpoint should extend this harness and its do() client rather than
// build its own ad hoc fxtest wiring.
type appHarness struct {
	baseURL    string
	client     *http.Client
	pool       *pgxpool.Pool
	adminToken string
	fxApp      *fxtest.App
	stopped    bool
}

// newAppHarness starts one fx.App per call, on its own free port, and
// registers its teardown with t.Cleanup. A test that needs to tune a
// setting the pool reads only at connect time (DATABASE_LOCK_TIMEOUT, for
// the transient-failure scenario) must t.Setenv it before calling this,
// since every harness gets its own pool.
//
// It also fetches a real wallet-admin token from Keycloak up front: every
// ticket-06 wallet test in this package predates ticket 07's auth
// requirement and calls do() without ever mentioning a token, so do() below
// applies this one by default unless a call explicitly sets (or blanks out)
// its own Authorization header - see do()'s doc comment.
func newAppHarness(t *testing.T) *appHarness {
	t.Helper()
	setAppEnv(t)

	var server *httpapi.Server
	var pool *pgxpool.Pool
	fxApp := fxtest.New(t, app.Modules, fx.Populate(&server, &pool))
	fxApp.RequireStart()
	h := &appHarness{
		baseURL:    "http://" + server.Addr(),
		client:     &http.Client{Timeout: 10 * time.Second},
		pool:       pool,
		adminToken: fetchToken(t, walletServiceClient()),
		fxApp:      fxApp,
	}
	t.Cleanup(func() {
		if !h.stopped {
			if err := h.stop(t, context.Background()); err != nil {
				t.Errorf("stop Fx app: %v", err)
			}
		}
	})
	return h
}

// stop lets the SQS lifecycle tests initiate the same Fx shutdown a SIGTERM
// causes while a message is running. Ordinary harness users keep the usual
// t.Cleanup stop; the bool prevents a second Stop after a test drove one.
func (h *appHarness) stop(t *testing.T, ctx context.Context) error {
	t.Helper()
	if h.stopped {
		return nil
	}
	h.stopped = true
	return h.fxApp.Stop(ctx)
}

// do issues one HTTP request against the running app and returns the
// response with its body already read, so callers never have to remember
// the drain-and-close dance themselves. body may be nil for a bodyless
// request.
//
// Every request carries h.adminToken as "Authorization: Bearer <token>" by
// default, since every /wallets* route now requires wallet-admin (ticket
// 07). A caller proving a specific auth scenario overrides this the same
// way it overrides any other header: pass "Authorization" in headers - an
// empty value removes the header entirely (the "missing token" scenario), a
// non-empty one replaces it (a different role's token, a tampered token, an
// expired one).
func (h *appHarness) do(t *testing.T, method, path string, headers map[string]string, body []byte) (*http.Response, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.baseURL+path, reader)
	requireNoError(t, err, "build request "+method+" "+path)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.adminToken)
	for k, v := range headers {
		if k == "Authorization" && v == "" {
			req.Header.Del("Authorization")
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := h.client.Do(req)
	requireNoError(t, err, method+" "+path)
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	requireNoError(t, err, "read response body for "+method+" "+path)
	return resp, respBody
}

// setAppEnv gives config.Load() everything it needs to build the same
// application docker-compose runs: a credential-free DATABASE_HOST/PORT/
// NAME/SSLMODE (see appConfigEnv), plus the wallet_app credentials file
// pg.New reads to add wallet_app's own userinfo on top (see
// internal/pg.AppDSN) - the same file walletAppPassword already reads for
// its own, separate purpose in this package.
func setAppEnv(t *testing.T) {
	t.Helper()
	for k, v := range appConfigEnv(t) {
		t.Setenv(k, v)
	}
	setEnvIfUnset(t, "SQS_ENDPOINT_URL", "http://localhost:4566")
	setEnvIfUnset(t, "AUTH_ISSUER_URL", keycloakIssuerURL())
	setEnvIfUnset(t, "AUTH_AUDIENCE", authAudience())
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
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

func setEnvIfUnset(t *testing.T, key, fallback string) {
	t.Helper()
	if os.Getenv(key) == "" {
		t.Setenv(key, fallback)
	}
}
