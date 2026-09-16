package config

import (
	"os"
	"strings"
	"testing"
)

// configEnvPrefixes covers every environment variable Load reads. The
// README tells whoever runs the integration suite from the host to export
// DATABASE_URL, SQS_ENDPOINT_URL and KEYCLOAK_PORT, so the shell running
// `go test ./...` right afterwards can easily carry values that change what
// Load returns. These tests assert defaults, so they clear that inherited
// environment first and let each case put back only what it means to test.
var configEnvPrefixes = []string{"DATABASE_", "POSTGRES_", "SQS_", "WAGER_", "AUTH_", "HTTP_", "FX_", "OUTBOX_", "REFERENCE_WORKER_", "LOG_", "METRICS_", "KEYCLOAK_", "MINISTACK_"}

func isolateConfigEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		for _, prefix := range configEnvPrefixes {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			t.Cleanup(func() { os.Setenv(name, value) })
			os.Unsetenv(name)
			break
		}
	}
}

func TestLoad_MissingRequiredValues(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("DATABASE_HOST", "")
	t.Setenv("DATABASE_NAME", "")
	t.Setenv("SQS_ENDPOINT_URL", "")
	t.Setenv("SQS_CONSUMER_ACCESS_KEY_ID", "")
	t.Setenv("SQS_CONSUMER_SECRET_ACCESS_KEY", "")
	t.Setenv("SQS_PUBLISHER_ACCESS_KEY_ID", "")
	t.Setenv("SQS_PUBLISHER_SECRET_ACCESS_KEY", "")
	t.Setenv("AUTH_ISSUER_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("want error for missing required config, got nil")
	}
}

func TestLoad_RejectsRootAccessKeys(t *testing.T) {
	base := validEnv(t)

	cases := map[string]string{
		"literal test key": "test",
		"12-digit key":     "000000000000",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			t.Setenv("SQS_CONSUMER_ACCESS_KEY_ID", key)

			_, err := Load()
			if err == nil {
				t.Fatalf("want error for root-shaped access key %q, got nil", key)
			}
		})
	}
}

func TestLoad_ValidEnvironment(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("want no error, got %v", err)
	}
	if cfg.Postgres.DSN != "postgres://localhost:5432/wallet?sslmode=disable" {
		t.Errorf("unexpected DSN: %q", cfg.Postgres.DSN)
	}
	if cfg.SQS.InputQueueName != "wager-transactions.fifo" {
		t.Errorf("unexpected default input queue name: %q", cfg.SQS.InputQueueName)
	}
	if cfg.Auth.Audience != "wallet-api" {
		t.Errorf("unexpected default audience: %q", cfg.Auth.Audience)
	}
}

func TestLoad_MissingIssuerURL(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}
	t.Setenv("AUTH_ISSUER_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("want error for missing AUTH_ISSUER_URL, got nil")
	}
}

func TestLoad_RejectsEmptyAudience(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}
	t.Setenv("AUTH_AUDIENCE", "")

	_, err := Load()
	if err == nil {
		t.Fatal("want error for empty AUTH_AUDIENCE, got nil")
	}
}

func TestLoad_DiscoveryURLDefaultsToIssuerURL(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}
	t.Setenv("AUTH_DISCOVERY_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("want no error, got %v", err)
	}
	if cfg.Auth.DiscoveryURL != cfg.Auth.IssuerURL {
		t.Errorf("DiscoveryURL = %q, want it to default to IssuerURL %q", cfg.Auth.DiscoveryURL, cfg.Auth.IssuerURL)
	}
}

func TestLoad_DiscoveryURLOverride(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}
	t.Setenv("AUTH_DISCOVERY_URL", "http://keycloak:8080/realms/wallet")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("want no error, got %v", err)
	}
	if cfg.Auth.DiscoveryURL != "http://keycloak:8080/realms/wallet" {
		t.Errorf("DiscoveryURL = %q, want the explicit override", cfg.Auth.DiscoveryURL)
	}
	if cfg.Auth.IssuerURL == cfg.Auth.DiscoveryURL {
		t.Error("IssuerURL and DiscoveryURL must stay independent settings")
	}
}

func TestLoad_RejectsUserinfoInDatabaseHost(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}
	t.Setenv("DATABASE_HOST", "wallet:wallet@localhost")

	_, err := Load()
	if err == nil {
		t.Fatal("want error when DATABASE_HOST carries userinfo, got nil")
	}
}

func TestLoad_RejectsNonPositiveDurations(t *testing.T) {
	cases := map[string]string{
		"HTTP_READ_TIMEOUT":                 "0s",
		"HTTP_WRITE_TIMEOUT":                "-1s",
		"HTTP_SHUTDOWN_TIMEOUT":             "0s",
		"HTTP_READINESS_TIMEOUT":            "-2s",
		"DATABASE_PING_TIMEOUT":             "0s",
		"DATABASE_LOCK_TIMEOUT":             "0s",
		"DATABASE_STATEMENT_TIMEOUT":        "-1s",
		"SQS_STARTUP_TIMEOUT":               "-5s",
		"OUTBOX_POLL_INTERVAL":              "0s",
		"OUTBOX_LEASE":                      "-1s",
		"OUTBOX_BATCH_SIZE":                 "0",
		"OUTBOX_RETRY_BASE":                 "0s",
		"OUTBOX_RETRY_MAX":                  "-1s",
		"REFERENCE_WORKER_POLL_INTERVAL":    "0s",
		"REFERENCE_WORKER_LEASE":            "-1s",
		"REFERENCE_WORKER_BATCH_SIZE":       "0",
		"REFERENCE_WORKER_RETRY_BASE":       "0s",
		"REFERENCE_WORKER_RETRY_MAX":        "-1s",
		"REFERENCE_WORKER_MAX_ATTEMPTS":     "0",
		"REFERENCE_WORKER_TTL":              "-1s",
		"REFERENCE_WORKER_SHUTDOWN_TIMEOUT": "0s",
		"AUTH_CLOCK_SKEW":                   "0s",
		"AUTH_DISCOVERY_TIMEOUT":            "-1s",
		"FX_STOP_TIMEOUT":                   "0s",
	}
	for key, value := range cases {
		t.Run(key+"="+value, func(t *testing.T) {
			for k, v := range validEnv(t) {
				t.Setenv(k, v)
			}
			t.Setenv(key, value)

			_, err := Load()
			if err == nil {
				t.Fatalf("want error for non-positive %s=%q, got nil", key, value)
			}
		})
	}
}

func TestLoad_RejectsOutboxRetryMaximumBelowBase(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}
	t.Setenv("OUTBOX_RETRY_BASE", "2s")
	t.Setenv("OUTBOX_RETRY_MAX", "1s")

	if _, err := Load(); err == nil {
		t.Fatal("want error when OUTBOX_RETRY_MAX is below OUTBOX_RETRY_BASE")
	}
}

func TestLoad_RejectsInsufficientStopTimeout(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}
	// FX_STOP_TIMEOUT equal to the configured internal deadlines must fail,
	// since the Fx-wide deadline would expire at the same instant the HTTP
	// drain is still allowed to run.
	t.Setenv("HTTP_SHUTDOWN_TIMEOUT", "20s")
	t.Setenv("SQS_CONSUMER_SHUTDOWN_TIMEOUT", "1s")
	t.Setenv("REFERENCE_WORKER_SHUTDOWN_TIMEOUT", "1s")
	t.Setenv("FX_STOP_TIMEOUT", "32s")

	_, err := Load()
	if err == nil {
		t.Fatal("want error when FX_STOP_TIMEOUT does not exceed internal stop deadlines, got nil")
	}
}

func TestLoad_RejectsConsumerShutdownBeforePollAndDrain(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "10s")
	t.Setenv("SQS_CONSUMER_SHUTDOWN_TIMEOUT", "15s")

	if _, err := Load(); err == nil {
		t.Fatal("want error when consumer shutdown cannot cover its poll and drain")
	}
}

func TestLoad_RequiresPostCancellationDrainBudget(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}
	t.Setenv("HTTP_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("SQS_CONSUMER_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("FX_STOP_TIMEOUT", "25s")

	if _, err := Load(); err == nil {
		t.Fatal("want error when FX_STOP_TIMEOUT leaves no post-cancellation drain budget")
	}
}

// The composition root arms fx.StopTimeout from StopTimeoutFromEnv, while
// Load validates the internal stop deadlines against its own value. A lower
// ceiling there would cut a graceful shutdown short under a configuration
// Load had accepted, so both must agree, with and without the variable set.
func TestStopTimeoutFromEnv_MatchesLoad(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if armed := StopTimeoutFromEnv(); armed != cfg.Fx.StopTimeout {
		t.Errorf("default: StopTimeoutFromEnv() = %s, Load() = %s, want equal", armed, cfg.Fx.StopTimeout)
	}

	t.Setenv("FX_STOP_TIMEOUT", "70s")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if armed := StopTimeoutFromEnv(); armed != cfg.Fx.StopTimeout {
		t.Errorf("explicit: StopTimeoutFromEnv() = %s, Load() = %s, want equal", armed, cfg.Fx.StopTimeout)
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	for k, v := range validEnv(t) {
		t.Setenv(k, v)
	}
	t.Setenv("LOG_LEVEL", "verbose")

	_, err := Load()
	if err == nil {
		t.Fatal("want error for invalid LOG_LEVEL, got nil")
	}
}

func validEnv(t *testing.T) map[string]string {
	t.Helper()
	isolateConfigEnv(t)
	return map[string]string{
		"DATABASE_HOST":                   "localhost",
		"DATABASE_NAME":                   "wallet",
		"SQS_ENDPOINT_URL":                "http://localhost:4566",
		"SQS_CONSUMER_ACCESS_KEY_ID":      "AKIACONSUMER",
		"SQS_CONSUMER_SECRET_ACCESS_KEY":  "consumer-secret",
		"SQS_PUBLISHER_ACCESS_KEY_ID":     "AKIAPUBLISHER",
		"SQS_PUBLISHER_SECRET_ACCESS_KEY": "publisher-secret",
		"AUTH_ISSUER_URL":                 "http://localhost:8081/realms/wallet",
	}
}
