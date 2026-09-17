//go:build integration || multiinstance

package testclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// KeycloakIssuerURL is both the app-under-test's own AUTH_ISSUER_URL and the
// base this package fetches tokens from directly.
func KeycloakIssuerURL() string {
	if v := EnvOrDefault("AUTH_ISSUER_URL", ""); v != "" {
		return v
	}
	return "http://localhost:" + EnvOrDefault("KEYCLOAK_PORT", "8081") + "/realms/wallet"
}

// KeycloakClient is one client_credentials fixture from
// deploy/keycloak/realm-wallet.json, with its id/secret overridable the same
// way every other credential these harnesses use is (.env.example documents
// the defaults - none of them are real secrets).
type KeycloakClient struct {
	ID     string
	Secret string
}

func WalletServiceClient() KeycloakClient {
	return KeycloakClient{
		ID:     EnvOrDefault("AUTH_TEST_WALLET_SERVICE_CLIENT_ID", "wallet-service"),
		Secret: EnvOrDefault("AUTH_TEST_WALLET_SERVICE_CLIENT_SECRET", "wallet-service-secret"),
	}
}

func ProviderAClient() KeycloakClient {
	return KeycloakClient{
		ID:     EnvOrDefault("AUTH_TEST_PROVIDER_A_CLIENT_ID", "provider-a"),
		Secret: EnvOrDefault("AUTH_TEST_PROVIDER_A_CLIENT_SECRET", "provider-a-secret"),
	}
}

func ProviderBClient() KeycloakClient {
	return KeycloakClient{
		ID:     EnvOrDefault("AUTH_TEST_PROVIDER_B_CLIENT_ID", "provider-b"),
		Secret: EnvOrDefault("AUTH_TEST_PROVIDER_B_CLIENT_SECRET", "provider-b-secret"),
	}
}

// FetchToken exchanges client credentials for a real access token via
// Keycloak's own token endpoint - never a token this package fabricates
// itself.
func FetchToken(ctx context.Context, t testing.TB, httpClient *http.Client, client KeycloakClient) string {
	t.Helper()

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {client.ID},
		"client_secret": {client.Secret},
	}
	tokenURL := strings.TrimSuffix(KeycloakIssuerURL(), "/") + "/protocol/openid-connect/token"
	resp, body := Do(ctx, t, httpClient, http.MethodPost, tokenURL, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, []byte(form.Encode()))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint for client %s: status = %d, want 200 - run scripts/wait-for-integration.sh first", client.ID, resp.StatusCode)
	}

	var decoded struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode token response for client %s: %v", client.ID, err)
	}
	if decoded.AccessToken == "" {
		t.Fatalf("empty access_token for client %s", client.ID)
	}
	return decoded.AccessToken
}
