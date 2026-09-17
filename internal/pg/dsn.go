package pg

import (
	"fmt"
	"net/url"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/envfile"
)

// AppDSN sets wallet_app's own least-privilege credentials as baseDSN's
// userinfo, so the application never connects with the migration owner's
// broader grants. baseDSN carries no userinfo of its own to overwrite. The
// password comes from credentialsFile, which deploy/postgres/provision.sh
// rewrites on every `docker compose up`, so the secret is generated at
// runtime and never versioned here.
func AppDSN(baseDSN, credentialsFile string) (string, error) {
	values, err := envfile.Read(credentialsFile)
	if err != nil {
		return "", fmt.Errorf("pg: read %s: %w", credentialsFile, err)
	}
	password, ok := values["WALLET_APP_PASSWORD"]
	if !ok || password == "" {
		return "", fmt.Errorf("pg: missing WALLET_APP_PASSWORD in %s - run `docker compose up postgres-provisioning` first", credentialsFile)
	}

	parsed, err := url.Parse(baseDSN)
	if err != nil {
		return "", fmt.Errorf("pg: parse database DSN: %w", err)
	}
	parsed.User = url.UserPassword("wallet_app", password)
	return parsed.String(), nil
}
