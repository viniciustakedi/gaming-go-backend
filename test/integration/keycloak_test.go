//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/test/testclient"
)

// tokenHTTPClient is the client every fetchToken call shares - a bounded
// timeout so a Keycloak that stops responding mid-suite fails the affected
// test instead of hanging it.
var tokenHTTPClient = testclient.NewHTTPClient(10 * time.Second)

// keycloakClient, keycloakIssuerURL, envOrDefault, fetchToken and the
// walletServiceClient/providerAClient/providerBClient fixtures are
// test/testclient's shared Keycloak client - test/multiinstance uses the same
// package. noRolesClient and shortLivedProviderClient stay local, since only
// this package exercises auth failure scenarios.
type keycloakClient = testclient.KeycloakClient

func envOrDefault(key, fallback string) string { return testclient.EnvOrDefault(key, fallback) }

func keycloakIssuerURL() string { return testclient.KeycloakIssuerURL() }

func authAudience() string {
	return envOrDefault("AUTH_AUDIENCE", "wallet-api")
}

func walletServiceClient() keycloakClient { return testclient.WalletServiceClient() }
func providerAClient() keycloakClient     { return testclient.ProviderAClient() }
func providerBClient() keycloakClient     { return testclient.ProviderBClient() }

// noRolesClient and shortLivedProviderClient fixtures from
// deploy/keycloak/realm-wallet.json, with their id/secret overridable the
// same way every other credential in this package is (.env.example
// documents the defaults - none of them are real secrets).
func noRolesClient() keycloakClient {
	return keycloakClient{
		ID:     envOrDefault("AUTH_TEST_NO_ROLES_CLIENT_ID", "no-roles-client"),
		Secret: envOrDefault("AUTH_TEST_NO_ROLES_CLIENT_SECRET", "no-roles-secret"),
	}
}

func shortLivedProviderClient() keycloakClient {
	return keycloakClient{
		ID:     envOrDefault("AUTH_TEST_SHORT_LIVED_CLIENT_ID", "provider-a-short-lived"),
		Secret: envOrDefault("AUTH_TEST_SHORT_LIVED_CLIENT_SECRET", "provider-a-short-lived-secret"),
	}
}

// fetchToken exchanges client credentials for a real access token via
// Keycloak's own token endpoint - never a token this package fabricates
// itself.
func fetchToken(t *testing.T, client keycloakClient) string {
	t.Helper()
	return testclient.FetchToken(t.Context(), t, tokenHTTPClient, client)
}
