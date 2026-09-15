//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// tokenHTTPClient is the client every fetchToken call shares - a bounded
// timeout so a Keycloak that stops responding mid-suite fails the affected
// test instead of hanging it (ticket 07 review: "fetchToken ... sem
// context.Context nem timeout").
var tokenHTTPClient = &http.Client{Timeout: 10 * time.Second}

// keycloakIssuerURL is both the app-under-test's own AUTH_ISSUER_URL (see
// setAppEnv in apphttp_test.go) and the base this package fetches tokens
// from directly - the ticket's own requirement ("O harness de teste obtém
// tokens reais do Keycloak por client"). Both sides run on the host in this
// seam 3a harness (fxtest, not the docker-compose app container), so both
// reach Keycloak the same way and see the same issuer - see
// docker-compose.yml's keycloak service comment for why that matters.
func keycloakIssuerURL() string {
	if v := os.Getenv("AUTH_ISSUER_URL"); v != "" {
		return v
	}
	port := os.Getenv("KEYCLOAK_PORT")
	if port == "" {
		port = "8081"
	}
	return "http://localhost:" + port + "/realms/wallet"
}

func authAudience() string {
	if v := os.Getenv("AUTH_AUDIENCE"); v != "" {
		return v
	}
	return "wallet-api"
}

// keycloakClient is one client_credentials fixture from
// deploy/keycloak/realm-wallet.json, with its id/secret overridable the same
// way every other credential in this package is (.env.example documents the
// defaults - none of them are real secrets, see that file's comment).
type keycloakClient struct {
	id     string
	secret string
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func walletServiceClient() keycloakClient {
	return keycloakClient{
		id:     envOrDefault("AUTH_TEST_WALLET_SERVICE_CLIENT_ID", "wallet-service"),
		secret: envOrDefault("AUTH_TEST_WALLET_SERVICE_CLIENT_SECRET", "wallet-service-secret"),
	}
}

func providerAClient() keycloakClient {
	return keycloakClient{
		id:     envOrDefault("AUTH_TEST_PROVIDER_A_CLIENT_ID", "provider-a"),
		secret: envOrDefault("AUTH_TEST_PROVIDER_A_CLIENT_SECRET", "provider-a-secret"),
	}
}

func providerBClient() keycloakClient {
	return keycloakClient{
		id:     envOrDefault("AUTH_TEST_PROVIDER_B_CLIENT_ID", "provider-b"),
		secret: envOrDefault("AUTH_TEST_PROVIDER_B_CLIENT_SECRET", "provider-b-secret"),
	}
}

func noRolesClient() keycloakClient {
	return keycloakClient{
		id:     envOrDefault("AUTH_TEST_NO_ROLES_CLIENT_ID", "no-roles-client"),
		secret: envOrDefault("AUTH_TEST_NO_ROLES_CLIENT_SECRET", "no-roles-secret"),
	}
}

func shortLivedProviderClient() keycloakClient {
	return keycloakClient{
		id:     envOrDefault("AUTH_TEST_SHORT_LIVED_CLIENT_ID", "provider-a-short-lived"),
		secret: envOrDefault("AUTH_TEST_SHORT_LIVED_CLIENT_SECRET", "provider-a-short-lived-secret"),
	}
}

// fetchToken exchanges client credentials for a real access token via
// Keycloak's own token endpoint - client_credentials grant, spec decision 7
// - never a token this package fabricates itself.
func fetchToken(t *testing.T, client keycloakClient) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {client.id},
		"client_secret": {client.secret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(keycloakIssuerURL(), "/")+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	requireNoError(t, err, "build token request for client "+client.id)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := tokenHTTPClient.Do(req)
	requireNoError(t, err, "fetch token for client "+client.id+" - run scripts/wait-for-integration.sh first")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint for client %s: status = %d, want 200", client.id, resp.StatusCode)
	}

	var decoded struct {
		AccessToken string `json:"access_token"`
	}
	requireNoError(t, json.NewDecoder(resp.Body).Decode(&decoded), "decode token response for client "+client.id)
	if decoded.AccessToken == "" {
		t.Fatalf("empty access_token for client %s", client.id)
	}
	return decoded.AccessToken
}
