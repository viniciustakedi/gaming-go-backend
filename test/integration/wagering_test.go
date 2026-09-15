//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/viniciustakedi/jungle-gaming-wallet/test/testclient"
)

// seam 3a - internal/httpapi's real HTTP contract for
// POST /wagering/transactions (ticket 08), driven through appHarness against
// the same Postgres, Keycloak and MiniStack every other test in this package
// uses. BET, WIN (with and without a reference), LOSS, REFUND and ROLLBACK
// are all in scope here (ticket 10) - only a reference that has not arrived
// yet, or is itself still PENDING_REFERENCE, is out of scope, left for
// ticket 11's durable PENDING_REFERENCE persistence and retry worker (see
// process_operation.go's ErrOperationNotSupported and
// wagering_reversals_test.go).

// wageringHTTPResponse, wageringBodyInput and wageringBody are
// test/testclient's shared DTOs and body builder (spec, seam 3: "o mesmo
// cliente de teste roda em dois harnesses") - test/multiinstance uses the
// same types.
type wageringHTTPResponse = testclient.WageringHTTPResponse

func decodeWageringResponse(t *testing.T, body []byte) wageringHTTPResponse {
	t.Helper()
	return testclient.DecodeWageringResponse(t, body)
}

type wageringBodyInput = testclient.WageringBodyInput

func wageringBody(in wageringBodyInput) []byte {
	return testclient.WageringBody(in)
}

// openWalletHTTP creates a fresh wallet with the wallet-admin token
// (appHarness's default) and returns its HTTP representation, for tests that
// need a real wallet to submit wagering operations against.
func openWalletHTTP(t *testing.T, h *appHarness, initialBalance string) walletHTTPResponse {
	t.Helper()
	resp, body := h.do(t, http.MethodPost, "/wallets", nil, openWalletBody(newUUID(t), initialBalance, testCurrency))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: POST /wallets status = %d, want 201, body = %s", resp.StatusCode, body)
	}
	return decodeWalletResponse(t, body)
}

// doWagering posts one wagering transaction as providerToken, with
// idempotencyKey (empty omits the header entirely, for the missing-key
// scenario) and an optional correlation id.
func doWagering(t *testing.T, h *appHarness, providerToken string, in wageringBodyInput, idempotencyKey, correlationID string) (*http.Response, []byte) {
	t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + providerToken}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	if correlationID != "" {
		headers["X-Correlation-Id"] = correlationID
	}
	return h.do(t, http.MethodPost, "/wagering/transactions", headers, wageringBody(in))
}

func providerAToken(t *testing.T) string { t.Helper(); return fetchToken(t, providerAClient()) }
func providerBToken(t *testing.T) string { t.Helper(); return fetchToken(t, providerBClient()) }

// wageringDuplicateAttemptsMetric reads GET /metrics - public, per spec
// ("Autenticação e autorização", "/metrics é público") - and parses out
// wagering_duplicate_attempts_total{channel="<channel>"}, so a test can
// confirm the duplicate-attempts counter actually moved by a hand-written
// amount, not just infer it from the replays it already counted itself.
func wageringDuplicateAttemptsMetric(t *testing.T, h *appHarness, channel string) float64 {
	t.Helper()
	resp, body := h.do(t, http.MethodGet, "/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	return testclient.DuplicateAttemptsMetric(t, body, channel)
}

func wageringOperationsMetric(t *testing.T, h *appHarness, channel, kind, status string) float64 {
	t.Helper()
	resp, body := h.do(t, http.MethodGet, "/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	prefix := fmt.Sprintf(`wagering_operations_total{channel="%s",kind="%s",status="%s"} `, channel, kind, status)
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, prefix) {
			value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
			requireNoError(t, err, "parse wagering_operations_total value")
			return value
		}
	}
	return 0
}

func queryWagerTransaction(t *testing.T, ctx context.Context, h *appHarness, id string) (status, failureCode string, resultingBalance *int64, found bool) {
	t.Helper()
	err := h.pool.QueryRow(ctx, `SELECT status, failure_code, resulting_balance FROM wager_transactions WHERE id = $1`, id).Scan(&status, &failureCode, &resultingBalance)
	if err == pgx.ErrNoRows {
		return "", "", nil, false
	}
	requireNoError(t, err, "query wager transaction")
	return status, failureCode, resultingBalance, true
}

func TestWageringBet_Processed_DebitsWalletAndRecordsLedgerAndOutbox(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	ledgerBaseline := countLedgerEntries(t, ctx, h, wallet.ID)            // the OPENING credit itself
	balanceEventsBaseline := len(queryOutboxEvents(t, ctx, h, wallet.ID)) // the OPENING's own WalletBalanceChanged
	token := providerAToken(t)
	correlationID := uniqueID("corr")

	resp, body := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), correlationID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "PROCESSED" || result.IdempotentReplay || result.Balance.Amount != "70.00" {
		t.Fatalf("result = %+v, want PROCESSED, not a replay, balance 70.00", result)
	}

	balance, version, found := queryWalletRow(t, ctx, h, wallet.ID)
	if !found || balance != 7000 || version != 2 {
		t.Fatalf("stored wallet = (balance %d, version %d), want (7000, 2)", balance, version)
	}
	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != ledgerBaseline+1 {
		t.Errorf("ledger entries = %d, want %d (opening credit + one debit)", got, ledgerBaseline+1)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}

	txEvents := queryOutboxEvents(t, ctx, h, result.TransactionID)
	if len(txEvents) != 1 || txEvents[0].eventType != "WagerTransactionProcessed" {
		t.Fatalf("outbox events for transaction = %+v, want exactly one WagerTransactionProcessed", txEvents)
	}
	var processed wagerTransactionProcessedPayload
	requireNoError(t, json.Unmarshal(txEvents[0].payload, &processed), "decode WagerTransactionProcessed")
	requireUTCEnvelope(t, processed.eventEnvelopeJSON, "WagerTransactionProcessed", result.TransactionID, correlationID)
	if processed.Data.Kind != "BET" || processed.Data.Origin != "EXTERNAL" || processed.Data.Status != "PROCESSED" {
		t.Errorf("processed data = %+v, want BET/EXTERNAL/PROCESSED", processed.Data)
	}

	balanceEvents := queryOutboxEvents(t, ctx, h, wallet.ID)
	if len(balanceEvents) != balanceEventsBaseline+1 {
		t.Fatalf("outbox events for wallet = %+v, want %d (the opening credit plus one more)", balanceEvents, balanceEventsBaseline+1)
	}
	latest := balanceEvents[len(balanceEvents)-1]
	if latest.eventType != "WalletBalanceChanged" {
		t.Fatalf("latest outbox event = %+v, want WalletBalanceChanged", latest)
	}
	var balanceChanged walletBalanceChangedPayload
	requireNoError(t, json.Unmarshal(latest.payload, &balanceChanged), "decode WalletBalanceChanged")
	if balanceChanged.Data.Direction != "DEBIT" || balanceChanged.Data.Money != (moneyJSON{Amount: "30.00", Currency: testCurrency}) {
		t.Errorf("balance changed data = %+v, want DEBIT of 30.00", balanceChanged.Data)
	}
}

func TestWageringWin_Processed_CreditsWallet(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "50.00")
	token := providerAToken(t)

	resp, body := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "WIN", Amount: "25.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Balance.Amount != "75.00" {
		t.Errorf("balance = %s, want 75.00", result.Balance.Amount)
	}

	balance, version, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 7500 || version != 2 {
		t.Errorf("stored wallet = (balance %d, version %d), want (7500, 2)", balance, version)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

func TestWageringLoss_Zero_NoLedgerNoVersionChange(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "50.00")
	ledgerBaseline := countLedgerEntries(t, ctx, h, wallet.ID)            // the OPENING credit itself
	balanceEventsBaseline := len(queryOutboxEvents(t, ctx, h, wallet.ID)) // the OPENING's own WalletBalanceChanged
	token := providerAToken(t)

	resp, body := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "LOSS", Amount: "0.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "PROCESSED" || result.Balance.Amount != "50.00" {
		t.Fatalf("result = %+v, want PROCESSED, balance unchanged at 50.00", result)
	}

	balance, version, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 5000 || version != 1 {
		t.Errorf("stored wallet = (balance %d, version %d), want (5000, 1) - LOSS must not move balance or version", balance, version)
	}
	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != ledgerBaseline {
		t.Errorf("ledger entries = %d, want %d (LOSS adds none)", got, ledgerBaseline)
	}

	txEvents := queryOutboxEvents(t, ctx, h, result.TransactionID)
	if len(txEvents) != 1 || txEvents[0].eventType != "WagerTransactionProcessed" {
		t.Fatalf("outbox events = %+v, want exactly one WagerTransactionProcessed", txEvents)
	}
	if events := queryOutboxEvents(t, ctx, h, wallet.ID); len(events) != balanceEventsBaseline {
		t.Errorf("WalletBalanceChanged events for wallet = %+v, want no new one for LOSS", events)
	}
}

func TestWageringBet_InsufficientFunds_RejectedAndAuditable(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "20.00")
	ledgerBaseline := countLedgerEntries(t, ctx, h, wallet.ID) // the OPENING credit itself
	token := providerAToken(t)

	resp, body := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "REJECTED" || result.FailureCode != "INSUFFICIENT_FUNDS" || result.Balance.Amount != "20.00" {
		t.Fatalf("result = %+v, want REJECTED/INSUFFICIENT_FUNDS/20.00", result)
	}

	balance, version, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 2000 || version != 1 {
		t.Errorf("stored wallet = (balance %d, version %d), want (2000, 1) - a rejected BET must not move the wallet", balance, version)
	}
	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != ledgerBaseline {
		t.Errorf("ledger entries = %d, want %d (a rejected BET adds none)", got, ledgerBaseline)
	}

	status, failureCode, resultingBalance, found := queryWagerTransaction(t, ctx, h, result.TransactionID)
	if !found || status != "REJECTED" || failureCode != "INSUFFICIENT_FUNDS" || resultingBalance == nil || *resultingBalance != 2000 {
		t.Fatalf("stored transaction = (status %s, failureCode %s, resultingBalance %v, found %v), want REJECTED/INSUFFICIENT_FUNDS/2000/true", status, failureCode, resultingBalance, found)
	}

	events := queryOutboxEvents(t, ctx, h, result.TransactionID)
	if len(events) != 1 || events[0].eventType != "WagerTransactionRejected" {
		t.Fatalf("outbox events = %+v, want exactly one WagerTransactionRejected", events)
	}
}

func TestWageringCorrectableInputs_RejectWithoutPersisting(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	otherPlayerWallet := openWalletHTTP(t, h, "10.00")
	token := providerAToken(t)

	base := func() wageringBodyInput {
		return wageringBodyInput{
			ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
			RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "10.00", Currency: testCurrency,
		}
	}
	ref := "some-ref"

	tests := []struct {
		name       string
		in         wageringBodyInput
		noKey      bool
		wantStatus int
		wantCode   string
	}{
		{name: "missing idempotency key", in: base(), noKey: true, wantStatus: http.StatusUnprocessableEntity, wantCode: "MISSING_IDEMPOTENCY_KEY"},
		{name: "invalid money format", in: func() wageringBodyInput { in := base(); in.Amount = "10.5"; return in }(), wantStatus: http.StatusBadRequest, wantCode: "INVALID_MONEY"},
		{name: "amount out of policy for BET", in: func() wageringBodyInput { in := base(); in.Amount = "0.00"; return in }(), wantStatus: http.StatusUnprocessableEntity, wantCode: "INVALID_AMOUNT_FOR_KIND"},
		{name: "OPENING not allowed", in: func() wageringBodyInput { in := base(); in.Kind = "OPENING"; return in }(), wantStatus: http.StatusUnprocessableEntity, wantCode: "KIND_NOT_ALLOWED"},
		{name: "reference forbidden on BET", in: func() wageringBodyInput { in := base(); in.ReferenceID = &ref; return in }(), wantStatus: http.StatusUnprocessableEntity, wantCode: "REFERENCE_NOT_ALLOWED"},
		{name: "wallet not found", in: func() wageringBodyInput { in := base(); in.WalletID = newUUID(t); return in }(), wantStatus: http.StatusNotFound, wantCode: "WALLET_NOT_FOUND"},
		{name: "wallet player mismatch", in: func() wageringBodyInput { in := base(); in.PlayerID = newUUID(t); return in }(), wantStatus: http.StatusUnprocessableEntity, wantCode: "WALLET_PLAYER_MISMATCH"},
		{name: "wallet currency mismatch", in: func() wageringBodyInput { in := base(); in.Currency = "USD"; return in }(), wantStatus: http.StatusUnprocessableEntity, wantCode: "WALLET_CURRENCY_MISMATCH"},
	}

	var before int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`).Scan(&before), "count external transactions before")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := "idem-" + uniqueID("k")
			if tt.noKey {
				key = ""
			}
			resp, body := doWagering(t, h, token, tt.in, key, "")
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, tt.wantStatus, body)
			}
			errResp := decodeErrorResponse(t, body)
			if errResp.Error.Code != tt.wantCode {
				t.Errorf("error code = %q, want %q", errResp.Error.Code, tt.wantCode)
			}
		})
	}
	_ = otherPlayerWallet

	var after int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`).Scan(&after), "count external transactions after")
	if after != before {
		t.Errorf("external transactions count changed from %d to %d - correctable input must persist nothing", before, after)
	}
}

func TestWageringReplay_ReturnsOriginalBalanceAfterFurtherMovement(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	ledgerBaseline := countLedgerEntries(t, ctx, h, wallet.ID) // the OPENING credit itself
	token := providerAToken(t)
	key := "idem-" + uniqueID("k")
	in := wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}

	first, firstBody := doWagering(t, h, token, in, key, "")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200, body = %s", first.StatusCode, firstBody)
	}
	firstResult := decodeWageringResponse(t, firstBody)

	// Move the wallet further so a naive replay would read a different
	// current balance than what was originally persisted.
	secondIn := wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "10.00", Currency: testCurrency,
	}
	second, secondBody := doWagering(t, h, token, secondIn, "idem-"+uniqueID("k"), "")
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200, body = %s", second.StatusCode, secondBody)
	}

	replay, replayBody := doWagering(t, h, token, in, key, "")
	if replay.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200, body = %s", replay.StatusCode, replayBody)
	}
	replayResult := decodeWageringResponse(t, replayBody)
	if !replayResult.IdempotentReplay || replayResult.TransactionID != firstResult.TransactionID || replayResult.Balance.Amount != "70.00" {
		t.Fatalf("replay result = %+v, want idempotentReplay=true, same transactionId, balance 70.00 (the balance at original processing)", replayResult)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 6000 {
		t.Fatalf("stored wallet balance = %d, want 6000 after both distinct bets", balance)
	}
	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != ledgerBaseline+2 {
		t.Errorf("ledger entries = %d, want %d (one per distinct bet, none for the replay)", got, ledgerBaseline+2)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

func TestWageringConflict_SameKeyDifferentContent_IdempotencyKeyReused(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	key := "idem-" + uniqueID("k")

	first, firstBody := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}, key, "")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200, body = %s", first.StatusCode, firstBody)
	}

	second, secondBody := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "31.00", Currency: testCurrency,
	}, key, "")
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second status = %d, want 409, body = %s", second.StatusCode, secondBody)
	}
	errResp := decodeErrorResponse(t, secondBody)
	if errResp.Error.Code != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("error code = %q, want IDEMPOTENCY_KEY_REUSED", errResp.Error.Code)
	}
}

func TestWageringConflict_DifferentKeySameExternalID_ExternalTransactionIDConflict(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	externalID := uniqueID("ext")

	first, firstBody := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: externalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200, body = %s", first.StatusCode, firstBody)
	}

	second, secondBody := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: externalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second status = %d, want 409, body = %s", second.StatusCode, secondBody)
	}
	errResp := decodeErrorResponse(t, secondBody)
	if errResp.Error.Code != "EXTERNAL_TRANSACTION_ID_CONFLICT" {
		t.Errorf("error code = %q, want EXTERNAL_TRANSACTION_ID_CONFLICT", errResp.Error.Code)
	}
}

func TestWageringProviderMismatch_ForbiddenWithNoEffect(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	tokenB := providerBToken(t)

	resp, body := doWagering(t, h, tokenB, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", resp.StatusCode, body)
	}

	balance, version, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 10000 || version != 1 {
		t.Errorf("stored wallet = (balance %d, version %d), want unchanged (10000, 1)", balance, version)
	}
}

func TestWageringIdempotencyKeys_ScopedByProvider(t *testing.T) {
	h := newAppHarness(t)
	walletA := openWalletHTTP(t, h, "100.00")
	walletB := openWalletHTTP(t, h, "100.00")
	tokenA, tokenB := providerAToken(t), providerBToken(t)
	sharedKey := "idem-" + uniqueID("shared")

	respA, bodyA := doWagering(t, h, tokenA, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: walletA.PlayerID, WalletID: walletA.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "10.00", Currency: testCurrency,
	}, sharedKey, "")
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("provider-a status = %d, want 200, body = %s", respA.StatusCode, bodyA)
	}
	resultA := decodeWageringResponse(t, bodyA)

	respB, bodyB := doWagering(t, h, tokenB, wageringBodyInput{
		ProviderID: "provider-b", ExternalID: uniqueID("ext"), PlayerID: walletB.PlayerID, WalletID: walletB.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "20.00", Currency: testCurrency,
	}, sharedKey, "")
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("provider-b status = %d, want 200 (same key, different provider, must not collide), body = %s", respB.StatusCode, bodyB)
	}
	resultB := decodeWageringResponse(t, bodyB)

	if resultA.IdempotentReplay || resultB.IdempotentReplay || resultA.TransactionID == resultB.TransactionID {
		t.Errorf("resultA = %+v, resultB = %+v, want two independent new transactions", resultA, resultB)
	}
}

func TestWagering50ParallelIdenticalBets_OneDebitFortyNineReplays(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "1000.00")
	ledgerBaseline := countLedgerEntries(t, ctx, h, wallet.ID) // the OPENING credit itself
	duplicatesBaseline := wageringDuplicateAttemptsMetric(t, h, "HTTP")
	token := providerAToken(t)
	key := "idem-" + uniqueID("k")
	in := wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}

	const attempts = 50
	results := make([]wageringHTTPResponse, attempts)
	statusCodes := make([]int, attempts)
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})
	ready.Add(attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			resp, body := doWagering(t, h, token, in, key, "")
			statusCodes[i] = resp.StatusCode
			if resp.StatusCode == http.StatusOK {
				results[i] = decodeWageringResponse(t, body)
			}
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	newAttempts, replays := 0, 0
	for i, code := range statusCodes {
		if code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want 200", i, code)
		}
		if results[i].IdempotentReplay {
			replays++
		} else {
			newAttempts++
		}
	}
	if newAttempts != 1 || replays != attempts-1 {
		t.Fatalf("newAttempts = %d, replays = %d, want exactly 1 and %d", newAttempts, replays, attempts-1)
	}
	if got := wageringDuplicateAttemptsMetric(t, h, "HTTP") - duplicatesBaseline; got != float64(attempts-1) {
		t.Errorf("wagering_duplicate_attempts_total{channel=\"HTTP\"} increased by %v, want %d", got, attempts-1)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 97000 {
		t.Fatalf("stored wallet balance = %d, want 97000 (a single 30.00 debit from 1000.00)", balance)
	}
	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != ledgerBaseline+1 {
		t.Errorf("ledger entries = %d, want %d despite 50 concurrent identical submissions", got, ledgerBaseline+1)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}

	// Resending after the fact must not change the result either.
	resendResp, resendBody := doWagering(t, h, token, in, key, "")
	if resendResp.StatusCode != http.StatusOK {
		t.Fatalf("resend status = %d, want 200, body = %s", resendResp.StatusCode, resendBody)
	}
	resend := decodeWageringResponse(t, resendBody)
	if !resend.IdempotentReplay || resend.Balance.Amount != "970.00" {
		t.Errorf("resend result = %+v, want a replay with balance 970.00", resend)
	}
}

func TestWageringTwoConcurrentBetsExceedingBalance_OneProcessedOneRejected(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	ledgerBaseline := countLedgerEntries(t, ctx, h, wallet.ID) // the OPENING credit itself
	token := providerAToken(t)

	inputs := [2]wageringBodyInput{
		{ProviderID: "provider-a", ExternalID: uniqueID("ext-1"), PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "80.00", Currency: testCurrency},
		{ProviderID: "provider-a", ExternalID: uniqueID("ext-2"), PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "80.00", Currency: testCurrency},
	}
	// Captured up front so the resend below can replay the exact same two
	// original requests - same key, same body - rather than fresh ones.
	keys := [2]string{"idem-" + uniqueID("k0"), "idem-" + uniqueID("k1")}
	results := make([]wageringHTTPResponse, 2)
	statusCodes := make([]int, 2)
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})
	ready.Add(2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			resp, body := doWagering(t, h, token, inputs[i], keys[i], "")
			statusCodes[i] = resp.StatusCode
			results[i] = decodeWageringResponse(t, body)
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	processed, rejected := 0, 0
	for i, code := range statusCodes {
		switch code {
		case http.StatusOK:
			processed++
			if results[i].Status != "PROCESSED" {
				t.Errorf("attempt %d: status 200 but body status = %q", i, results[i].Status)
			}
		case http.StatusUnprocessableEntity:
			rejected++
			if results[i].FailureCode != "INSUFFICIENT_FUNDS" {
				t.Errorf("attempt %d: 422 but failureCode = %q, want INSUFFICIENT_FUNDS", i, results[i].FailureCode)
			}
		default:
			t.Errorf("attempt %d: unexpected status code %d", i, code)
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("processed = %d, rejected = %d, want exactly 1 and 1", processed, rejected)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 2000 {
		t.Fatalf("stored wallet balance = %d, want 2000 (\"20.00\")", balance)
	}
	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != ledgerBaseline+1 {
		t.Errorf("ledger entries = %d, want %d", got, ledgerBaseline+1)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}

	// Resending the exact same two original requests - same key, same body
	// - must replay each one's own original result, not process it again.
	for i := 0; i < 2; i++ {
		resp, body := doWagering(t, h, token, inputs[i], keys[i], "")
		if resp.StatusCode != statusCodes[i] {
			t.Errorf("resend %d status = %d, want original status %d", i, resp.StatusCode, statusCodes[i])
		}
		replay := decodeWageringResponse(t, body)
		if !replay.IdempotentReplay || replay.TransactionID != results[i].TransactionID || replay.Status != results[i].Status ||
			replay.FailureCode != results[i].FailureCode || replay.Balance != results[i].Balance {
			t.Errorf("resend %d = %+v, want a replay of the original result %+v", i, replay, results[i])
		}
	}
	balanceAfterResend, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balanceAfterResend != balance {
		t.Errorf("balance after resend = %d, want unchanged %d", balanceAfterResend, balance)
	}
	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != ledgerBaseline+1 {
		t.Errorf("ledger entries after resend = %d, want unchanged %d", got, ledgerBaseline+1)
	}
}

func TestWageringDistinctWallets_ProcessInParallelWithoutSerialization(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	token := providerAToken(t)

	const wallets = 5
	walletResponses := make([]walletHTTPResponse, wallets)
	for i := range walletResponses {
		walletResponses[i] = openWalletHTTP(t, h, "100.00")
	}

	results := make([]int, wallets)
	var wg sync.WaitGroup
	start := make(chan struct{})
	startedAt := time.Now()
	for i := 0; i < wallets; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, _ := doWagering(t, h, token, wageringBodyInput{
				ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: walletResponses[i].PlayerID, WalletID: walletResponses[i].ID,
				RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "10.00", Currency: testCurrency,
			}, "idem-"+uniqueID("k"), "")
			results[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	elapsed := time.Since(startedAt)

	for i, code := range results {
		if code != http.StatusOK {
			t.Errorf("wallet %d status = %d, want 200", i, code)
		}
	}
	// A loose upper bound: if wallets were serialized behind one lock, this
	// would tend toward `wallets` sequential round trips; distinct wallets
	// use distinct row locks and must not exhibit that.
	if elapsed > 5*time.Second {
		t.Errorf("elapsed = %s processing %d distinct wallets concurrently, want them to run without cross-wallet serialization", elapsed, wallets)
	}

	for i := range walletResponses {
		balance, _, _ := queryWalletRow(t, ctx, h, walletResponses[i].ID)
		if balance != 9000 {
			t.Errorf("wallet %d balance = %d, want 9000", i, balance)
		}
		if net := netLedgerBalance(t, ctx, h, walletResponses[i].ID); net != balance {
			t.Errorf("wallet %d: stored balance %d does not match ledger net %d", i, balance, net)
		}
	}
}

// TestWageringTransientFailure_ServiceUnavailableWithRetryAfterAndNoEffect
// mirrors TestOpenWallet_TransientDatabaseFailure_PersistsNothing: a real
// lock_timeout, forced by holding an ACCESS EXCLUSIVE lock on wallets from a
// separate connection while the app - configured with a short
// DATABASE_LOCK_TIMEOUT for this test only - tries to lock the wallet row.
func TestWageringTransientFailure_ServiceUnavailableWithRetryAfterAndNoEffect(t *testing.T) {
	t.Setenv("DATABASE_LOCK_TIMEOUT", "300ms")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)

	lockConn := connectOwner(t, ctx)
	tx, err := lockConn.Begin(ctx)
	requireNoError(t, err, "begin locking transaction")
	_, err = tx.Exec(ctx, "LOCK TABLE wallets IN ACCESS EXCLUSIVE MODE")
	requireNoError(t, err, "lock wallets table")
	defer func() { _ = tx.Rollback(ctx) }()

	resp, body := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "10.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("want a Retry-After header on 503")
	}
	errResp := decodeErrorResponse(t, body)
	if errResp.Error.Code != "TEMPORARILY_UNAVAILABLE" {
		t.Errorf("error code = %q, want TEMPORARILY_UNAVAILABLE", errResp.Error.Code)
	}

	requireNoError(t, tx.Rollback(ctx), "release the wallets lock")
	time.Sleep(50 * time.Millisecond)

	balance, version, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 10000 || version != 1 {
		t.Errorf("stored wallet = (balance %d, version %d), want unchanged (10000, 1) after a transient failure", balance, version)
	}
	var count int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND origin = 'EXTERNAL'`, wallet.ID).Scan(&count), "count external wager transactions")
	if count != 0 {
		t.Errorf("external wager_transactions for wallet = %d, want 0 - a transient failure must persist nothing", count)
	}
}
