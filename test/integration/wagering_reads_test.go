//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// seam 3a - internal/httpapi's real HTTP contract for
// GET /wagering/transactions/:transactionId and
// GET /providers/:providerId/wagering/transactions/:externalTransactionId
// (ticket 09), driven through appHarness against the same Postgres,
// Keycloak and MiniStack every other test in this package uses. Provider
// isolation between provider-a and provider-b for POST and for idempotency
// keys is already proven end to end by TestWageringProviderMismatch_ForbiddenWithNoEffect
// and TestWageringIdempotencyKeys_ScopedByProvider in wagering_test.go; this
// file covers the two read routes these tests do not touch.

type transactionDetailHTTPResponse struct {
	TransactionID                  string     `json:"transactionId"`
	ExternalTransactionID          string     `json:"externalTransactionId"`
	ProviderID                     string     `json:"providerId"`
	PlayerID                       string     `json:"playerId"`
	WalletID                       string     `json:"walletId"`
	RoundID                        string     `json:"roundId"`
	GameID                         string     `json:"gameId"`
	Kind                           string     `json:"kind"`
	Origin                         string     `json:"origin"`
	Money                          moneyJSON  `json:"money"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId"`
	ReferenceTransactionID         string     `json:"referenceTransactionId"`
	Status                         string     `json:"status"`
	FailureCode                    string     `json:"failureCode"`
	ResultingBalance               *moneyJSON `json:"resultingBalance"`
	Attempts                       int        `json:"attempts"`
	CreatedAt                      string     `json:"createdAt"`
	UpdatedAt                      string     `json:"updatedAt"`
}

func decodeTransactionDetailResponse(t *testing.T, body []byte) transactionDetailHTTPResponse {
	t.Helper()
	var resp transactionDetailHTTPResponse
	requireNoError(t, json.Unmarshal(body, &resp), "decode transaction detail response: "+string(body))
	assertUTCTimestamp(t, "createdAt", resp.CreatedAt)
	assertUTCTimestamp(t, "updatedAt", resp.UpdatedAt)
	return resp
}

// assertUTCTimestamp proves the re-review fix (Major #2) against the real
// Postgres codec, not just the fake repository: whatever zone the process
// itself runs in, a TIMESTAMPTZ read back through the handler always comes
// out as RFC 3339 UTC, terminated in "Z" - never a local offset.
func assertUTCTimestamp(t *testing.T, field, value string) {
	t.Helper()
	if !strings.HasSuffix(value, "Z") {
		t.Errorf("%s = %q, want it to end in \"Z\" (UTC)", field, value)
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		t.Errorf("%s = %q, want a valid RFC 3339 timestamp: %v", field, value, err)
	}
}

// getWageringTransaction issues GET /wagering/transactions/:id with token,
// or - since h.do defaults to the admin token when no Authorization header
// is given at all - removes the header entirely for the "missing token"
// scenario when token is empty (see appHarness.do's doc comment).
func getWageringTransaction(t *testing.T, h *appHarness, token, transactionID string) (*http.Response, []byte) {
	t.Helper()
	headers := map[string]string{"Authorization": ""}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return h.do(t, http.MethodGet, "/wagering/transactions/"+transactionID, headers, nil)
}

func getProviderWageringTransaction(t *testing.T, h *appHarness, token, providerID, externalID string) (*http.Response, []byte) {
	t.Helper()
	headers := map[string]string{"Authorization": ""}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return h.do(t, http.MethodGet, "/providers/"+providerID+"/wagering/transactions/"+externalID, headers, nil)
}

// openingTransactionID reads the internal id of wallet w's own OPENING row -
// every wallet opened with a positive balance gets exactly one (spec:
// "abertura com saldo inicial positivo credita ... e cria a transação
// OPENING").
func openingTransactionID(t *testing.T, ctx context.Context, h *appHarness, walletID string) string {
	t.Helper()
	var id string
	err := h.pool.QueryRow(ctx, `SELECT id FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING'`, walletID).Scan(&id)
	requireNoError(t, err, "query OPENING transaction id")
	return id
}

func TestGetWageringTransactionByID_Owner_ReturnsFullRecord(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	tokenA := providerAToken(t)
	externalID := uniqueID("ext")

	resp, body := doWagering(t, h, tokenA, wageringBodyInput{
		providerID: "provider-a", externalID: externalID, playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "30.00", currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup POST status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	submitted := decodeWageringResponse(t, body)

	getResp, getBody := getWageringTransaction(t, h, tokenA, submitted.TransactionID)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", getResp.StatusCode, getBody)
	}
	detail := decodeTransactionDetailResponse(t, getBody)
	if detail.TransactionID != submitted.TransactionID || detail.ExternalTransactionID != externalID ||
		detail.ProviderID != "provider-a" || detail.PlayerID != wallet.PlayerID || detail.WalletID != wallet.ID ||
		detail.Kind != "BET" || detail.Origin != "EXTERNAL" || detail.Status != "PROCESSED" ||
		detail.Money != (moneyJSON{Amount: "30.00", Currency: testCurrency}) ||
		detail.ResultingBalance == nil || *detail.ResultingBalance != (moneyJSON{Amount: "70.00", Currency: testCurrency}) {
		t.Errorf("detail = %+v, want the full BET record for provider-a's own submission", detail)
	}
}

func TestGetWageringTransactionByID_OtherProvider_NotFound(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	tokenA, tokenB := providerAToken(t), providerBToken(t)

	resp, body := doWagering(t, h, tokenA, wageringBodyInput{
		providerID: "provider-a", externalID: uniqueID("ext"), playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "10.00", currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup POST status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	submitted := decodeWageringResponse(t, body)

	getResp, getBody := getWageringTransaction(t, h, tokenB, submitted.TransactionID)
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("provider-b reading provider-a's transaction: status = %d, want 404, body = %s", getResp.StatusCode, getBody)
	}
	errResp := decodeErrorResponse(t, getBody)
	if errResp.Error.Code != "TRANSACTION_NOT_FOUND" {
		t.Errorf("error code = %q, want TRANSACTION_NOT_FOUND", errResp.Error.Code)
	}
}

// TestGetWageringTransactionByID_InternalOpening_NotFoundForProvider proves
// a provider can never read the wallet's own internal OPENING transaction,
// even though it shares the same walletId as its own operations (spec: "as
// ... de origem interna devolvem 404, sem revelar existência").
func TestGetWageringTransactionByID_InternalOpening_NotFoundForProvider(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	tokenA := providerAToken(t)

	openingID := openingTransactionID(t, ctx, h, wallet.ID)

	resp, body := getWageringTransaction(t, h, tokenA, openingID)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", resp.StatusCode, body)
	}
}

func TestGetWageringTransactionByID_Admin_SeesOwnProviderRowAndOpening(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	tokenA := providerAToken(t)

	resp, body := doWagering(t, h, tokenA, wageringBodyInput{
		providerID: "provider-a", externalID: uniqueID("ext"), playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "10.00", currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup POST status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	submitted := decodeWageringResponse(t, body)

	// h.do defaults to the admin token when no Authorization override is given.
	betResp, betBody := h.do(t, http.MethodGet, "/wagering/transactions/"+submitted.TransactionID, nil, nil)
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("admin reading provider-a's BET: status = %d, want 200, body = %s", betResp.StatusCode, betBody)
	}

	openingID := openingTransactionID(t, ctx, h, wallet.ID)
	openingResp, openingBody := h.do(t, http.MethodGet, "/wagering/transactions/"+openingID, nil, nil)
	if openingResp.StatusCode != http.StatusOK {
		t.Fatalf("admin reading OPENING: status = %d, want 200, body = %s", openingResp.StatusCode, openingBody)
	}
	openingDetail := decodeTransactionDetailResponse(t, openingBody)
	if openingDetail.Kind != "OPENING" || openingDetail.Origin != "INTERNAL" || openingDetail.ProviderID != "" {
		t.Errorf("opening detail = %+v, want kind OPENING, origin INTERNAL, no providerId", openingDetail)
	}
}

func TestGetWageringTransactionByID_MalformedID_Returns400(t *testing.T) {
	h := newAppHarness(t)
	resp, body := getWageringTransaction(t, h, providerAToken(t), "not-a-uuid")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
	errResp := decodeErrorResponse(t, body)
	if errResp.Error.Code != "INVALID_REQUEST" {
		t.Errorf("error code = %q, want INVALID_REQUEST", errResp.Error.Code)
	}
}

// TestGetWageringTransactionByID_NonCanonicalUUID_Returns400 proves the
// spec's requirement that the path id be a canonical UUID - lowercase,
// hyphenated - end to end through the real mux (review: uuid.Parse alone
// accepted uppercase and other non-canonical spellings).
func TestGetWageringTransactionByID_NonCanonicalUUID_Returns400(t *testing.T) {
	h := newAppHarness(t)
	canonical := newUUID(t)

	cases := []struct {
		name string
		id   string
	}{
		{"uppercase", strings.ToUpper(canonical)},
		{"no hyphens", strings.ReplaceAll(canonical, "-", "")},
		{"with spaces", canonical + " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := getWageringTransaction(t, h, providerAToken(t), tc.id)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
			}
			errResp := decodeErrorResponse(t, body)
			if errResp.Error.Code != "INVALID_REQUEST" {
				t.Errorf("error code = %q, want INVALID_REQUEST", errResp.Error.Code)
			}
		})
	}
}

func TestGetWageringTransactionByID_NonexistentID_Returns404(t *testing.T) {
	h := newAppHarness(t)
	resp, body := getWageringTransaction(t, h, providerAToken(t), newUUID(t))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", resp.StatusCode, body)
	}
}

func TestGetProviderWageringTransaction_Owner_ReturnsFullRecord(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	tokenA := providerAToken(t)
	externalID := uniqueID("ext")

	resp, body := doWagering(t, h, tokenA, wageringBodyInput{
		providerID: "provider-a", externalID: externalID, playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "15.00", currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup POST status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	submitted := decodeWageringResponse(t, body)

	getResp, getBody := getProviderWageringTransaction(t, h, tokenA, "provider-a", externalID)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", getResp.StatusCode, getBody)
	}
	detail := decodeTransactionDetailResponse(t, getBody)
	if detail.TransactionID != submitted.TransactionID || detail.ExternalTransactionID != externalID {
		t.Errorf("detail = %+v, want transactionId %q and externalTransactionId %q", detail, submitted.TransactionID, externalID)
	}
}

// TestGetProviderWageringTransaction_MismatchedProvider_Forbidden proves
// provider-b cannot read provider-a's transaction through the provider-scoped
// route either (spec: "provider com providerId diferente devolve 403").
func TestGetProviderWageringTransaction_MismatchedProvider_Forbidden(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	tokenA, tokenB := providerAToken(t), providerBToken(t)
	externalID := uniqueID("ext")

	resp, body := doWagering(t, h, tokenA, wageringBodyInput{
		providerID: "provider-a", externalID: externalID, playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "10.00", currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup POST status = %d, want 200, body = %s", resp.StatusCode, body)
	}

	getResp, getBody := getProviderWageringTransaction(t, h, tokenB, "provider-a", externalID)
	if getResp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", getResp.StatusCode, getBody)
	}
}

func TestGetProviderWageringTransaction_Admin_AnyProvider(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	tokenA := providerAToken(t)
	externalID := uniqueID("ext")

	resp, body := doWagering(t, h, tokenA, wageringBodyInput{
		providerID: "provider-a", externalID: externalID, playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "10.00", currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup POST status = %d, want 200, body = %s", resp.StatusCode, body)
	}

	// h.do defaults to the admin token when no Authorization override is given.
	getResp, getBody := h.do(t, http.MethodGet, "/providers/provider-a/wagering/transactions/"+externalID, nil, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", getResp.StatusCode, getBody)
	}
}

func TestGetProviderWageringTransaction_MalformedExternalID_Returns400(t *testing.T) {
	h := newAppHarness(t)
	overlong := strings.Repeat("x", 256)

	resp, body := getProviderWageringTransaction(t, h, providerAToken(t), "provider-a", overlong)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
	errResp := decodeErrorResponse(t, body)
	if errResp.Error.Code != "INVALID_REQUEST" {
		t.Errorf("error code = %q, want INVALID_REQUEST", errResp.Error.Code)
	}
}

func TestGetProviderWageringTransaction_Nonexistent_Returns404(t *testing.T) {
	h := newAppHarness(t)
	resp, body := getProviderWageringTransaction(t, h, providerAToken(t), "provider-a", uniqueID("ext"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", resp.StatusCode, body)
	}
}

// specTransactionDetailKeys is every key the spec's "registro completo"
// requires in the wire body, present or not - a review finding
// (`omitempty` silently dropping a key instead of sending it as null) is
// invisible to transactionDetailHTTPResponse's typed decode above, so these
// two tests decode into a bare map instead.
var specTransactionDetailKeys = []string{
	"transactionId", "externalTransactionId", "providerId", "playerId", "walletId",
	"roundId", "gameId", "kind", "origin", "money",
	"referenceExternalTransactionId", "referenceTransactionId", "status", "failureCode",
	"resultingBalance", "attempts", "nextAttemptAt", "pendingExpiresAt", "createdAt", "updatedAt",
}

func decodeTransactionDetailMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	requireNoError(t, json.Unmarshal(body, &decoded), "decode transaction detail response: "+string(body))
	for _, key := range specTransactionDetailKeys {
		if _, ok := decoded[key]; !ok {
			t.Errorf("response missing key %q, want it present (null when not applicable)", key)
		}
	}
	return decoded
}

func assertNullKeys(t *testing.T, decoded map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if decoded[key] != nil {
			t.Errorf("key %q = %v, want null", key, decoded[key])
		}
	}
}

// TestGetWageringTransactionByID_NoReference_NullKeysPresent proves a plain
// BET - no reference at all - still sends referenceExternalTransactionId,
// referenceTransactionId, failureCode, nextAttemptAt and pendingExpiresAt as
// explicit null rather than omitting them (review: "omitempty elimina
// campos condicionais/nulos").
func TestGetWageringTransactionByID_NoReference_NullKeysPresent(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	tokenA := providerAToken(t)

	resp, body := doWagering(t, h, tokenA, wageringBodyInput{
		providerID: "provider-a", externalID: uniqueID("ext"), playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "10.00", currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup POST status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	submitted := decodeWageringResponse(t, body)

	getResp, getBody := getWageringTransaction(t, h, tokenA, submitted.TransactionID)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", getResp.StatusCode, getBody)
	}
	decoded := decodeTransactionDetailMap(t, getBody)
	assertNullKeys(t, decoded, "referenceExternalTransactionId", "referenceTransactionId", "failureCode", "nextAttemptAt", "pendingExpiresAt")
}

// TestGetWageringTransactionByID_AdminOpening_NullKeysPresent proves the
// same fix for the INTERNAL OPENING row wallet-admin alone can read:
// externalTransactionId, providerId, roundId and gameId - none of which an
// OPENING row ever carries - are sent as explicit null.
func TestGetWageringTransactionByID_AdminOpening_NullKeysPresent(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	openingID := openingTransactionID(t, ctx, h, wallet.ID)

	// h.do defaults to the admin token when no Authorization override is given.
	resp, body := h.do(t, http.MethodGet, "/wagering/transactions/"+openingID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	decoded := decodeTransactionDetailMap(t, body)
	assertNullKeys(t, decoded, "externalTransactionId", "providerId", "roundId", "gameId",
		"referenceExternalTransactionId", "referenceTransactionId", "failureCode", "nextAttemptAt", "pendingExpiresAt")
}

// TestWageringReads_MissingToken_Returns401 covers both read routes'
// authentication requirement, mirroring the write route's own coverage in
// auth_test.go.
func TestWageringReads_MissingToken_Returns401(t *testing.T) {
	h := newAppHarness(t)

	resp, body := getWageringTransaction(t, h, "", newUUID(t))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /wagering/transactions/:id without token: status = %d, want 401, body = %s", resp.StatusCode, body)
	}

	resp, body = getProviderWageringTransaction(t, h, "", "provider-a", uniqueID("ext"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /providers/:providerId/wagering/transactions/:id without token: status = %d, want 401, body = %s", resp.StatusCode, body)
	}
}
