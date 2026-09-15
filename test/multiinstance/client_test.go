//go:build multiinstance

package multiinstance

import (
	"net/http"
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/test/testclient"
)

const testCurrency = "BRL"

var httpClient = testclient.NewHTTPClient(10 * time.Second)

// wageringBodyInput, wageringHTTPResponse and walletHTTPResponse are
// test/testclient's shared DTOs (spec, seam 3: "o mesmo cliente de teste
// roda em dois harnesses") - test/integration uses the same types.
type wageringBodyInput = testclient.WageringBodyInput
type wageringHTTPResponse = testclient.WageringHTTPResponse
type walletHTTPResponse = testclient.WalletHTTPResponse

func uniqueID(prefix string) string { return testclient.UniqueID(prefix) }
func newUUID(t *testing.T) string   { t.Helper(); return testclient.NewUUID(t) }

func decodeWageringResponse(t *testing.T, body []byte) wageringHTTPResponse {
	t.Helper()
	return testclient.DecodeWageringResponse(t, body)
}

func decodeWalletResponse(t *testing.T, body []byte) walletHTTPResponse {
	t.Helper()
	return testclient.DecodeWalletResponse(t, body)
}

// doAt issues one HTTP request against inst, bound to t's own context.
// Unlike test/integration's single-baseURL appHarness, every call here names
// its instance explicitly - the whole point of seam 3b is that a scenario's
// calls can land on *different* wallet-service processes while still
// hitting the same Postgres, MiniStack and Keycloak.
func doAt(t *testing.T, inst *instance, method, path, token string, body []byte) (*http.Response, []byte) {
	t.Helper()
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return testclient.Do(t.Context(), t, httpClient, method, inst.baseURL+path, headers, body)
}

func doWageringAt(t *testing.T, inst *instance, token string, in wageringBodyInput, idempotencyKey string) (*http.Response, []byte) {
	t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + token}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	return testclient.Do(t.Context(), t, httpClient, http.MethodPost, inst.baseURL+"/wagering/transactions", headers, testclient.WageringBody(in))
}

func openWalletAt(t *testing.T, inst *instance, adminToken, playerID, amount string) walletHTTPResponse {
	t.Helper()
	resp, body := doAt(t, inst, http.MethodPost, "/wallets", adminToken, testclient.OpenWalletBody(playerID, amount, testCurrency))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: POST /wallets (instance %s) status = %d, want 201, body = %s", inst.name, resp.StatusCode, body)
	}
	return decodeWalletResponse(t, body)
}

// duplicateAttemptsMetric reads GET /metrics on inst - public, per spec
// ("Autenticação e autorização", "/metrics é público") - and parses out
// wagering_duplicate_attempts_total{channel="<channel>"}, the independent
// source of truth a duplicate-submission scenario checks against (spec,
// Testing Decisions: "Os testes de duplicidade provam que as entradas
// repetidas chegaram de fato à aplicação, pela métrica de duplicatas").
func duplicateAttemptsMetric(t *testing.T, inst *instance, channel string) float64 {
	t.Helper()
	resp, body := doAt(t, inst, http.MethodGet, "/metrics", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics (instance %s) status = %d, want 200", inst.name, resp.StatusCode)
	}
	return testclient.DuplicateAttemptsMetric(t, body, channel)
}
