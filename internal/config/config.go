// Package config loads and validates the service configuration from the
// environment. A missing or malformed value fails fast at construction time,
// before any Fx lifecycle hook runs, so the process never starts half-wired.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting the service needs, grouped by the component
// that owns it.
type Config struct {
	HTTP     HTTPConfig
	Log      LogConfig
	Postgres PostgresConfig
	SQS      SQSConfig
	Auth     AuthConfig
	Fx       FxConfig
}

type HTTPConfig struct {
	Addr             string
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	ShutdownTimeout  time.Duration
	ReadinessTimeout time.Duration
}

type LogConfig struct {
	Level string
}

type PostgresConfig struct {
	// Host, Port, Name and SSLMode carry no secret: the app process never
	// receives the migration owner's credentials (spec/ticket 06 review:
	// "o serviço app não recebe nenhuma credencial do papel dono"). DSN is
	// assembled from these four alone, so it can never carry userinfo
	// either - only migrate and postgres-provisioning are handed the
	// owner's own DATABASE_URL, read directly from the environment, never
	// through this struct.
	Host               string
	Port               string
	Name               string
	SSLMode            string
	DSN                string
	PingTimeout        time.Duration
	MaxConns           int32
	AppCredentialsFile string
	LockTimeout        time.Duration
	StatementTimeout   time.Duration
}

type SQSConfig struct {
	EndpointURL string
	Region      string

	InputQueueName  string
	DLQQueueName    string
	OutputQueueName string

	ConsumerAccessKeyID      string
	ConsumerSecretAccessKey  string
	PublisherAccessKeyID     string
	PublisherSecretAccessKey string

	StartupTimeout time.Duration
}

type FxConfig struct {
	StopTimeout time.Duration
}

type AuthConfig struct {
	// IssuerURL is the exact "iss" claim every accepted token must carry -
	// internal/auth's verifier rejects a token whose issuer does not match
	// this string byte for byte, regardless of which host DiscoveryURL
	// below actually reaches.
	IssuerURL string
	// DiscoveryURL is the OIDC discovery endpoint's base
	// (discoveryURL + "/.well-known/openid-configuration"), used only to
	// fetch the provider's configuration and JWKS - never compared against
	// a token's "iss". It defaults to IssuerURL, the right value whenever a
	// single host reaches Keycloak (local `go test`, test/integration), and
	// is set to Keycloak's internal Compose address for the app container,
	// which cannot reach IssuerURL's host-facing address (ticket 07:
	// "separe issuer esperado de URL de descoberta e JWKS").
	DiscoveryURL string
	// Audience is the "aud" every accepted token must carry - the audience
	// mapper on each Keycloak client in this realm stamps it there (spec,
	// decision 7).
	Audience string
	// ClockSkew is the tolerance internal/auth's verifier grants a token's
	// expiry against this process's own clock.
	ClockSkew time.Duration
	// DiscoveryTimeout bounds how long internal/auth retries OIDC discovery
	// on start - see internal/auth.RegisterLifecycle.
	DiscoveryTimeout time.Duration
}

// rootAccessKeyID12Digits matches MiniStack's other bypass form: any
// 12-digit numeric access key is treated as root, same as the literal "test"
// key. Rejecting both at config load time means a misconfigured deployment
// can never hand the application root credentials, even by accident.
var rootAccessKeyID12Digits = regexp.MustCompile(`^\d{12}$`)

// StopTimeoutFromEnv reads FX_STOP_TIMEOUT with the same default Load uses,
// without running full validation. fx.StopTimeout must be set as a static
// option before fx.New builds the container, which is before any
// fx.Provide constructor - including Load itself - has run, so this is the
// one setting the composition root needs outside the normal DI flow. Actual
// validation of this value still happens through Load; an invalid duration
// here silently falls back to the default and the real error surfaces from
// Load moments later, when the Fx graph is built.
func StopTimeoutFromEnv() time.Duration {
	var discarded []error
	return getDuration("FX_STOP_TIMEOUT", 30*time.Second, &discarded)
}

// Load reads the configuration from the process environment and validates
// it. It returns an error - never panics - so the Fx constructor that wraps
// it can abort startup with a clear message.
func Load() (Config, error) {
	var cfg Config
	var errs []error

	cfg.HTTP.Addr = getEnv("HTTP_ADDR", ":8080")
	cfg.HTTP.ReadTimeout = getDuration("HTTP_READ_TIMEOUT", 5*time.Second, &errs)
	cfg.HTTP.WriteTimeout = getDuration("HTTP_WRITE_TIMEOUT", 10*time.Second, &errs)
	cfg.HTTP.ShutdownTimeout = getDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second, &errs)
	cfg.HTTP.ReadinessTimeout = getDuration("HTTP_READINESS_TIMEOUT", 3*time.Second, &errs)

	cfg.Log.Level = getEnv("LOG_LEVEL", "info")
	if !isValidLogLevel(cfg.Log.Level) {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: invalid value %q, want one of debug|info|warn|error", cfg.Log.Level))
	}

	cfg.Postgres.Host = getEnv("DATABASE_HOST", "")
	if cfg.Postgres.Host == "" {
		errs = append(errs, errors.New("DATABASE_HOST: required"))
	} else if strings.Contains(cfg.Postgres.Host, "@") {
		// The only way userinfo could ever reach the DSN this package
		// assembles below: reject it here rather than let it flow through
		// silently (ticket 06 review: "falhar se o app receber userinfo no
		// DSN"). The app derives its actual connection exclusively from
		// AppCredentialsFile - see internal/pg.AppDSN.
		errs = append(errs, errors.New("DATABASE_HOST: must not contain credentials (userinfo); the app derives its Postgres connection only from DATABASE_APP_CREDENTIALS_FILE"))
	}
	cfg.Postgres.Port = getEnv("DATABASE_PORT", "5432")
	cfg.Postgres.Name = getEnv("DATABASE_NAME", "")
	if cfg.Postgres.Name == "" {
		errs = append(errs, errors.New("DATABASE_NAME: required"))
	}
	cfg.Postgres.SSLMode = getEnv("DATABASE_SSLMODE", "disable")
	if cfg.Postgres.Host != "" && !strings.Contains(cfg.Postgres.Host, "@") && cfg.Postgres.Name != "" {
		cfg.Postgres.DSN = fmt.Sprintf("postgres://%s:%s/%s?sslmode=%s", cfg.Postgres.Host, cfg.Postgres.Port, cfg.Postgres.Name, cfg.Postgres.SSLMode)
	}
	cfg.Postgres.PingTimeout = getDuration("DATABASE_PING_TIMEOUT", 5*time.Second, &errs)
	cfg.Postgres.MaxConns = getInt32("DATABASE_MAX_CONNS", 10, &errs)
	// The app never receives the migration owner's credentials at all: DSN
	// above carries no userinfo, and pg.New/pg.AppDSN adds wallet_app's
	// least-privilege role on top of it, with the password read only from
	// this file - generated at runtime by deploy/postgres/provision.sh,
	// never versioned. See internal/pg.AppDSN and README.md's "Credenciais
	// do Postgres".
	cfg.Postgres.AppCredentialsFile = getEnv("DATABASE_APP_CREDENTIALS_FILE", "deploy/postgres/.runtime/credentials.env")
	// Session-level GUCs (spec: "As sessões Postgres usam lock_timeout e
	// statement_timeout, e estourar qualquer um deles é falha transitória"),
	// applied per-connection by pg.New through pgx's RuntimeParams.
	cfg.Postgres.LockTimeout = getDuration("DATABASE_LOCK_TIMEOUT", 3*time.Second, &errs)
	cfg.Postgres.StatementTimeout = getDuration("DATABASE_STATEMENT_TIMEOUT", 5*time.Second, &errs)

	cfg.SQS.EndpointURL = getEnv("SQS_ENDPOINT_URL", "")
	if cfg.SQS.EndpointURL == "" {
		errs = append(errs, errors.New("SQS_ENDPOINT_URL: required"))
	}
	cfg.SQS.Region = getEnv("SQS_REGION", "us-east-1")
	cfg.SQS.InputQueueName = getEnv("SQS_INPUT_QUEUE_NAME", "wager-transactions.fifo")
	cfg.SQS.DLQQueueName = getEnv("SQS_DLQ_QUEUE_NAME", "wager-transactions-dlq.fifo")
	cfg.SQS.OutputQueueName = getEnv("SQS_OUTPUT_QUEUE_NAME", "wallet-events.fifo")

	cfg.SQS.ConsumerAccessKeyID = getEnv("SQS_CONSUMER_ACCESS_KEY_ID", "")
	cfg.SQS.ConsumerSecretAccessKey = getEnv("SQS_CONSUMER_SECRET_ACCESS_KEY", "")
	requireRoleCredentials(&errs, "SQS_CONSUMER", cfg.SQS.ConsumerAccessKeyID, cfg.SQS.ConsumerSecretAccessKey)

	cfg.SQS.PublisherAccessKeyID = getEnv("SQS_PUBLISHER_ACCESS_KEY_ID", "")
	cfg.SQS.PublisherSecretAccessKey = getEnv("SQS_PUBLISHER_SECRET_ACCESS_KEY", "")
	requireRoleCredentials(&errs, "SQS_PUBLISHER", cfg.SQS.PublisherAccessKeyID, cfg.SQS.PublisherSecretAccessKey)

	cfg.SQS.StartupTimeout = getDuration("SQS_STARTUP_TIMEOUT", 10*time.Second, &errs)

	cfg.Auth.IssuerURL = getEnv("AUTH_ISSUER_URL", "")
	if cfg.Auth.IssuerURL == "" {
		errs = append(errs, errors.New("AUTH_ISSUER_URL: required"))
	}
	cfg.Auth.DiscoveryURL = getEnv("AUTH_DISCOVERY_URL", "")
	if cfg.Auth.DiscoveryURL == "" {
		cfg.Auth.DiscoveryURL = cfg.Auth.IssuerURL
	}
	cfg.Auth.Audience = getEnv("AUTH_AUDIENCE", "wallet-api")
	if cfg.Auth.Audience == "" {
		errs = append(errs, errors.New("AUTH_AUDIENCE: must not be empty"))
	}
	cfg.Auth.ClockSkew = getDuration("AUTH_CLOCK_SKEW", 5*time.Second, &errs)
	// Keycloak's own boot - including the realm import docker-compose.yml's
	// keycloak service runs on every cold start - routinely takes longer
	// than every other dependency this process waits on, so the default
	// here is generous rather than matched to SQS_STARTUP_TIMEOUT's 10s.
	cfg.Auth.DiscoveryTimeout = getDuration("AUTH_DISCOVERY_TIMEOUT", 45*time.Second, &errs)

	cfg.Fx.StopTimeout = getDuration("FX_STOP_TIMEOUT", 30*time.Second, &errs)

	// internalStopDeadline sums every stop-side deadline the shutdown
	// sequence already waits on before the pools close - today just the HTTP
	// drain. fx.StopTimeout must exceed that sum, or the Fx-wide deadline can
	// expire while a component is still draining within its own, smaller
	// budget (spec: "fx.StopTimeout configurado acima da soma dos prazos
	// internos"). Extend this sum if a future stop hook gains its own
	// configurable deadline.
	internalStopDeadline := cfg.HTTP.ShutdownTimeout
	if cfg.Fx.StopTimeout <= internalStopDeadline {
		errs = append(errs, fmt.Errorf("FX_STOP_TIMEOUT: must be greater than the sum of internal stop deadlines (%s), got %s", internalStopDeadline, cfg.Fx.StopTimeout))
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// requireRoleCredentials rejects missing keys and, just as importantly,
// rejects MiniStack's two root bypass shapes: the literal "test" key and any
// 12-digit numeric key. Both skip every IAM policy in MiniStack, so refusing
// them here is what makes "a aplicação nunca usa as chaves root" a property
// the process enforces, not just a convention the operator has to remember.
func requireRoleCredentials(errs *[]error, prefix, accessKeyID, secretAccessKey string) {
	if accessKeyID == "" {
		*errs = append(*errs, fmt.Errorf("%s_ACCESS_KEY_ID: required", prefix))
	} else if accessKeyID == "test" || rootAccessKeyID12Digits.MatchString(accessKeyID) {
		*errs = append(*errs, fmt.Errorf("%s_ACCESS_KEY_ID: looks like a MiniStack root key, refusing to use it", prefix))
	}
	if secretAccessKey == "" {
		*errs = append(*errs, fmt.Errorf("%s_SECRET_ACCESS_KEY: required", prefix))
	}
}

func isValidLogLevel(level string) bool {
	switch strings.ToLower(level) {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// getDuration requires a strictly positive duration: zero blocks any I/O
// timeout from ever firing, and a negative one makes context.WithTimeout
// build an already-expired context, so both are rejected the same way a
// malformed value is - Load fails fast instead of the process discovering
// the bad value only when the first timeout misbehaves at runtime.
func getDuration(key string, fallback time.Duration, errs *[]error) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: invalid duration %q: %w", key, v, err))
		return fallback
	}
	if d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s: must be a strictly positive duration, got %q", key, v))
		return fallback
	}
	return d
}

func getInt32(key string, fallback int32, errs *[]error) int32 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n <= 0 {
		*errs = append(*errs, fmt.Errorf("%s: invalid positive integer %q", key, v))
		return fallback
	}
	return int32(n)
}
