//go:build multiinstance

package multiinstance

import (
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/test/testclient"
)

// tokenHTTPClient, keycloakClient, fetchToken and the client fixtures below
// are test/testclient's shared Keycloak client (spec, seam 3: "o mesmo
// cliente de teste roda em dois harnesses") - test/integration uses the same
// package.
var tokenHTTPClient = testclient.NewHTTPClient(10 * time.Second)

type keycloakClient = testclient.KeycloakClient

func providerAClient() keycloakClient     { return testclient.ProviderAClient() }
func walletServiceClient() keycloakClient { return testclient.WalletServiceClient() }

func fetchToken(t *testing.T, client keycloakClient) string {
	t.Helper()
	return testclient.FetchToken(t.Context(), t, tokenHTTPClient, client)
}
