package pg

import (
	"fmt"
	"net/url"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/envfile"
)

// AppDSN sets wallet_app's own least-privilege credentials as baseDSN's
// userinfo, so the application never connects to Postgres with the
// migration owner's broader grants (spec: "a aplicação usa outro [papel],
// com apenas os grants necessários"). baseDSN itself never carries any
// credential (internal/config.Load assembles it from host/port/database/
// sslmode alone, with no userinfo of its own to overwrite). The password is
// read from credentialsFile, the file deploy/postgres/provision.sh writes
// at every `docker compose up` (see README.md, "Credenciais do Postgres") -
// the same one test/integration's own appDSN helper reads, so both ever
// authenticate as wallet_app the same way, with a runtime-generated secret
// never versioned or hardcoded here.
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
