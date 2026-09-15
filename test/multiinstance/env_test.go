//go:build multiinstance

package multiinstance

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/envfile"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/pg"
	"github.com/viniciustakedi/jungle-gaming-wallet/test/testclient"
)

const defaultOwnerDSN = "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable"

// ownerDSN mirrors test/integration's own helper of the same name: the
// migration owner's connection string, read from DATABASE_URL with the same
// default docker-compose.yml's postgres service exposes. It is used here
// only to source appDSN's non-secret host/port/database/sslmode and this
// package's own read-only assertion queries - never handed to a
// wallet-service instance, which only ever receives wallet_app's
// credentials (see baseEnv below).
func ownerDSN() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return defaultOwnerDSN
}

// envOrDefault and keycloakIssuerURL are test/testclient's shared helpers
// (spec, seam 3: "o mesmo cliente de teste roda em dois harnesses") -
// test/integration uses the same package.
func envOrDefault(key, fallback string) string { return testclient.EnvOrDefault(key, fallback) }

func keycloakIssuerURL() string { return testclient.KeycloakIssuerURL() }

func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

func postgresCredentialsFile() string {
	return repoPath("deploy", "postgres", ".runtime", "credentials.env")
}

func sqsCredentialsFile() string {
	return repoPath("deploy", "ministack", ".runtime", "app-credentials.env")
}

// appDSN reuses internal/pg.AppDSN - the same production helper pg.New
// itself calls - to connect to Postgres as wallet_app, so this package's
// own read-only assertions (wallet balance, ledger sums) run under the same
// least-privilege role as the instances under test, not the migration
// owner.
func appDSN(t *testing.T) string {
	t.Helper()
	dsn, err := pg.AppDSN(ownerDSN(), postgresCredentialsFile())
	if err != nil {
		t.Fatalf("%v - run scripts/wait-for-integration.sh first", err)
	}
	return dsn
}

// baseEnv is the configuration every instance in a trio shares - the same
// non-secret settings docker-compose.yml's app service receives, sourced
// the same way test/integration/apphttp_test.go's setAppEnv is (host/port/
// database/sslmode parsed out of DATABASE_URL, wallet_app's password file,
// the SQS consumer/publisher role keys the provisioning one-shot already
// wrote). Only HTTP_ADDR differs per instance - see startInstance.
func baseEnv(t *testing.T) map[string]string {
	t.Helper()

	u, err := url.Parse(ownerDSN())
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

	if _, err := envfile.Read(postgresCredentialsFile()); err != nil {
		t.Fatalf("%v - run scripts/wait-for-integration.sh first", err)
	}

	sqsCreds, err := envfile.Read(sqsCredentialsFile())
	if err != nil {
		t.Fatalf("%v - run scripts/wait-for-integration.sh first", err)
	}
	for _, key := range []string{"SQS_CONSUMER_ACCESS_KEY_ID", "SQS_CONSUMER_SECRET_ACCESS_KEY", "SQS_PUBLISHER_ACCESS_KEY_ID", "SQS_PUBLISHER_SECRET_ACCESS_KEY"} {
		if sqsCreds[key] == "" {
			t.Fatalf("missing %s in %s - run scripts/wait-for-integration.sh first", key, sqsCredentialsFile())
		}
	}

	issuerURL := envOrDefault("AUTH_ISSUER_URL", keycloakIssuerURL())

	return map[string]string{
		"LOG_LEVEL":                       "info",
		"DATABASE_HOST":                   u.Hostname(),
		"DATABASE_PORT":                   port,
		"DATABASE_NAME":                   strings.TrimPrefix(u.Path, "/"),
		"DATABASE_SSLMODE":                sslmode,
		"DATABASE_APP_CREDENTIALS_FILE":   postgresCredentialsFile(),
		"SQS_ENDPOINT_URL":                envOrDefault("SQS_ENDPOINT_URL", "http://localhost:4566"),
		"AUTH_ISSUER_URL":                 issuerURL,
		"AUTH_DISCOVERY_URL":              issuerURL,
		"AUTH_AUDIENCE":                   envOrDefault("AUTH_AUDIENCE", "wallet-api"),
		"SQS_CONSUMER_ACCESS_KEY_ID":      sqsCreds["SQS_CONSUMER_ACCESS_KEY_ID"],
		"SQS_CONSUMER_SECRET_ACCESS_KEY":  sqsCreds["SQS_CONSUMER_SECRET_ACCESS_KEY"],
		"SQS_PUBLISHER_ACCESS_KEY_ID":     sqsCreds["SQS_PUBLISHER_ACCESS_KEY_ID"],
		"SQS_PUBLISHER_SECRET_ACCESS_KEY": sqsCreds["SQS_PUBLISHER_SECRET_ACCESS_KEY"],
		"SQS_CONSUMER_ENABLED":            "false",
	}
}
