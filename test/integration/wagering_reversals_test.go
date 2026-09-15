//go:build integration

package integration

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

// seam 3a - ticket 10's own slice of POST /wagering/transactions: REFUND,
// ROLLBACK and a referenced WIN, each resolved against a reference that has
// already arrived and reached a terminal status (spec, "Regras das
// operações e referências"). A reference that has not arrived at all, or
// has arrived but is itself still PENDING_REFERENCE, is out of this
// ticket's scope - ticket 11's durable PENDING_REFERENCE persistence and
// retry worker - and is only exercised here as the one documented "not
// implemented" case (TestWageringReversal_MissingReference_NotImplemented).

// mustBetHTTP submits a processed BET for wallet in round and returns its
// own externalId - the value every reversal test below references - along
// with the decoded response, failing the test unless it actually processed.
func mustBetHTTP(t *testing.T, h *appHarness, token string, wallet walletHTTPResponse, roundID, amount string) (externalID string, result wageringHTTPResponse) {
	t.Helper()
	externalID = uniqueID("ext")
	resp, body := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: externalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: roundID, GameID: "game-1", Kind: "BET", Amount: amount, Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup BET status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	return externalID, decodeWageringResponse(t, body)
}

// mustWinHTTP is mustBetHTTP's WIN sibling, for ROLLBACK-of-WIN scenarios.
func mustWinHTTP(t *testing.T, h *appHarness, token string, wallet walletHTTPResponse, roundID, amount string) (externalID string, result wageringHTTPResponse) {
	t.Helper()
	externalID = uniqueID("ext")
	resp, body := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: externalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: roundID, GameID: "game-1", Kind: "WIN", Amount: amount, Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup WIN status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	return externalID, decodeWageringResponse(t, body)
}

// doReversal submits one REFUND, ROLLBACK or referenced WIN against wallet,
// naming referenceID as its referenceExternalTransactionId, with a fresh
// externalId and idempotency key each call.
func doReversal(t *testing.T, h *appHarness, token string, wallet walletHTTPResponse, kind, roundID, amount, referenceID string) (*http.Response, []byte) {
	t.Helper()
	ref := referenceID
	return doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: roundID, GameID: "game-1", Kind: kind, Amount: amount, Currency: testCurrency, ReferenceID: &ref,
	}, "idem-"+uniqueID("k"), "")
}

func queryReferenceTransactionID(t *testing.T, ctx context.Context, h *appHarness, id string) string {
	t.Helper()
	var ref *string
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT reference_transaction_id FROM wager_transactions WHERE id = $1`, id).Scan(&ref), "query reference_transaction_id")
	if ref == nil {
		return ""
	}
	return *ref
}

func TestWageringRefund_OfBet_CreditsAndPersistsResolvedReference(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	betExternalID, betResult := mustBetHTTP(t, h, token, wallet, round, "30.00")
	if betResult.Balance.Amount != "70.00" {
		t.Fatalf("bet balance = %s, want 70.00", betResult.Balance.Amount)
	}

	resp, body := doReversal(t, h, token, wallet, "REFUND", round, "30.00", betExternalID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "PROCESSED" || result.Balance.Amount != "100.00" {
		t.Fatalf("result = %+v, want PROCESSED/100.00", result)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 10000 {
		t.Fatalf("stored balance = %d, want 10000", balance)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
	if refTxID := queryReferenceTransactionID(t, ctx, h, result.TransactionID); refTxID != betResult.TransactionID {
		t.Errorf("stored referenceTransactionId = %q, want the bet's own id %q", refTxID, betResult.TransactionID)
	}

	events := queryOutboxEvents(t, ctx, h, result.TransactionID)
	if len(events) != 1 || events[0].eventType != "WagerTransactionProcessed" {
		t.Fatalf("outbox events = %+v, want exactly one WagerTransactionProcessed", events)
	}
}

func TestWageringRollback_OfBet_Credits(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	betExternalID, _ := mustBetHTTP(t, h, token, wallet, round, "30.00")

	resp, body := doReversal(t, h, token, wallet, "ROLLBACK", round, "30.00", betExternalID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "PROCESSED" || result.Balance.Amount != "100.00" {
		t.Fatalf("result = %+v, want PROCESSED/100.00 (ROLLBACK of a BET credits)", result)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 10000 {
		t.Fatalf("stored balance = %d, want 10000", balance)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

func TestWageringRollback_OfWin_Debits(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "50.00")
	token := providerAToken(t)
	round := uniqueID("round")

	winExternalID, winResult := mustWinHTTP(t, h, token, wallet, round, "25.00")
	if winResult.Balance.Amount != "75.00" {
		t.Fatalf("win balance = %s, want 75.00", winResult.Balance.Amount)
	}

	resp, body := doReversal(t, h, token, wallet, "ROLLBACK", round, "25.00", winExternalID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "PROCESSED" || result.Balance.Amount != "50.00" {
		t.Fatalf("result = %+v, want PROCESSED/50.00 (ROLLBACK of a WIN debits)", result)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 5000 {
		t.Fatalf("stored balance = %d, want 5000", balance)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

// TestWageringRollback_OfRefund_DebitsAndBlocksNewRefund covers "ROLLBACK de
// REFUND volta a debitar e impede novo reembolso": the original REFUND row
// stays PROCESSED forever (transitions out of a terminal status are
// rejected), so the BET it reversed still counts as having its one
// successful reversal, and a second REFUND against it is REJECTED with
// REFERENCE_ALREADY_REVERSED even after the REFUND itself was rolled back.
func TestWageringRollback_OfRefund_DebitsAndBlocksNewRefund(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	betExternalID, _ := mustBetHTTP(t, h, token, wallet, round, "30.00")

	refundExternalID := uniqueID("ext")
	refundResp, refundBody := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: refundExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: round, GameID: "game-1", Kind: "REFUND", Amount: "30.00", Currency: testCurrency, ReferenceID: &betExternalID,
	}, "idem-"+uniqueID("k"), "")
	if refundResp.StatusCode != http.StatusOK {
		t.Fatalf("REFUND status = %d, want 200, body = %s", refundResp.StatusCode, refundBody)
	}
	refundResult := decodeWageringResponse(t, refundBody)
	if refundResult.Balance.Amount != "100.00" {
		t.Fatalf("REFUND balance = %s, want 100.00", refundResult.Balance.Amount)
	}

	rollbackResp, rollbackBody := doReversal(t, h, token, wallet, "ROLLBACK", round, "30.00", refundExternalID)
	if rollbackResp.StatusCode != http.StatusOK {
		t.Fatalf("ROLLBACK status = %d, want 200, body = %s", rollbackResp.StatusCode, rollbackBody)
	}
	rollbackResult := decodeWageringResponse(t, rollbackBody)
	if rollbackResult.Status != "PROCESSED" || rollbackResult.Balance.Amount != "70.00" {
		t.Fatalf("ROLLBACK result = %+v, want PROCESSED/70.00 (undoes the refund)", rollbackResult)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 7000 {
		t.Fatalf("stored balance = %d, want 7000", balance)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}

	secondRefundResp, secondRefundBody := doReversal(t, h, token, wallet, "REFUND", round, "30.00", betExternalID)
	if secondRefundResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("second REFUND status = %d, want 422, body = %s", secondRefundResp.StatusCode, secondRefundBody)
	}
	secondRefundResult := decodeWageringResponse(t, secondRefundBody)
	if secondRefundResult.Status != "REJECTED" || secondRefundResult.FailureCode != "REFERENCE_ALREADY_REVERSED" {
		t.Fatalf("second REFUND result = %+v, want REJECTED/REFERENCE_ALREADY_REVERSED", secondRefundResult)
	}

	balanceAfter, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balanceAfter != balance {
		t.Errorf("stored balance after blocked second refund = %d, want unchanged %d", balanceAfter, balance)
	}
}

func TestWageringWinWithReference_ValidatesBetSameRound_Credits(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "70.00")
	token := providerAToken(t)
	round := uniqueID("round")

	betExternalID, _ := mustBetHTTP(t, h, token, wallet, round, "30.00")

	resp, body := doReversal(t, h, token, wallet, "WIN", round, "50.00", betExternalID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	// wallet started at 70.00, the BET debited 30.00 to 40.00, and this WIN
	// credits 50.00 on top of that.
	if result.Status != "PROCESSED" || result.Balance.Amount != "90.00" {
		t.Fatalf("result = %+v, want PROCESSED/90.00", result)
	}
	if refTxID := queryReferenceTransactionID(t, ctx, h, result.TransactionID); refTxID == "" {
		t.Errorf("stored referenceTransactionId is empty, want the bet's resolved id")
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

func TestWageringWinWithReference_DifferentRound_ReferenceMismatch(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "70.00")
	token := providerAToken(t)

	betExternalID, _ := mustBetHTTP(t, h, token, wallet, uniqueID("round"), "30.00")

	resp, body := doReversal(t, h, token, wallet, "WIN", uniqueID("round"), "50.00", betExternalID)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "REJECTED" || result.FailureCode != "REFERENCE_MISMATCH" {
		t.Fatalf("result = %+v, want REJECTED/REFERENCE_MISMATCH", result)
	}
}

func TestWageringReversal_SecondReversalOfSameBet_ReferenceAlreadyReversed(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	betExternalID, _ := mustBetHTTP(t, h, token, wallet, round, "30.00")

	first, firstBody := doReversal(t, h, token, wallet, "REFUND", round, "30.00", betExternalID)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first REFUND status = %d, want 200, body = %s", first.StatusCode, firstBody)
	}

	second, secondBody := doReversal(t, h, token, wallet, "ROLLBACK", round, "30.00", betExternalID)
	if second.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("second reversal (ROLLBACK) status = %d, want 422, body = %s", second.StatusCode, secondBody)
	}
	result := decodeWageringResponse(t, secondBody)
	if result.Status != "REJECTED" || result.FailureCode != "REFERENCE_ALREADY_REVERSED" {
		t.Fatalf("result = %+v, want REJECTED/REFERENCE_ALREADY_REVERSED", result)
	}

	events := queryOutboxEvents(t, ctx, h, result.TransactionID)
	if len(events) != 1 || events[0].eventType != "WagerTransactionRejected" {
		t.Fatalf("outbox events = %+v, want exactly one WagerTransactionRejected", events)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 10000 {
		t.Fatalf("stored balance = %d, want 10000 (only the first reversal moved money)", balance)
	}
}

// TestWageringReversal_RejectedReversal_ReplayIsStable proves a durable
// reversal rejection replays exactly like any other persisted rejection:
// resending the same idempotency key returns the original transactionId,
// status and failureCode, marked as a replay, with no further effect.
func TestWageringReversal_RejectedReversal_ReplayIsStable(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	betExternalID, _ := mustBetHTTP(t, h, token, wallet, round, "30.00")

	first, firstBody := doReversal(t, h, token, wallet, "REFUND", round, "30.00", betExternalID)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first REFUND status = %d, want 200, body = %s", first.StatusCode, firstBody)
	}

	key := "idem-" + uniqueID("k")
	ref := betExternalID
	rejectedIn := wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: round, GameID: "game-1", Kind: "ROLLBACK", Amount: "30.00", Currency: testCurrency, ReferenceID: &ref,
	}
	original, originalBody := doWagering(t, h, token, rejectedIn, key, "")
	if original.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("original ROLLBACK status = %d, want 422, body = %s", original.StatusCode, originalBody)
	}
	originalResult := decodeWageringResponse(t, originalBody)
	if originalResult.Status != "REJECTED" || originalResult.FailureCode != "REFERENCE_ALREADY_REVERSED" {
		t.Fatalf("original result = %+v, want REJECTED/REFERENCE_ALREADY_REVERSED", originalResult)
	}

	replay, replayBody := doWagering(t, h, token, rejectedIn, key, "")
	if replay.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("replay status = %d, want 422, body = %s", replay.StatusCode, replayBody)
	}
	replayResult := decodeWageringResponse(t, replayBody)
	if !replayResult.IdempotentReplay || replayResult.TransactionID != originalResult.TransactionID ||
		replayResult.Status != originalResult.Status || replayResult.FailureCode != originalResult.FailureCode ||
		replayResult.Balance != originalResult.Balance {
		t.Fatalf("replay result = %+v, want a stable replay of %+v", replayResult, originalResult)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 10000 {
		t.Fatalf("stored balance = %d, want 10000 (only the first REFUND ever moved money)", balance)
	}
}

func TestWageringRollback_OfRollback_ReferenceKindNotReversible(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	betExternalID, _ := mustBetHTTP(t, h, token, wallet, round, "30.00")

	firstRollbackExternalID := uniqueID("ext")
	first, firstBody := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: firstRollbackExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: round, GameID: "game-1", Kind: "ROLLBACK", Amount: "30.00", Currency: testCurrency, ReferenceID: &betExternalID,
	}, "idem-"+uniqueID("k"), "")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first ROLLBACK status = %d, want 200, body = %s", first.StatusCode, firstBody)
	}

	second, secondBody := doReversal(t, h, token, wallet, "ROLLBACK", round, "30.00", firstRollbackExternalID)
	if second.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("ROLLBACK of ROLLBACK status = %d, want 422, body = %s", second.StatusCode, secondBody)
	}
	result := decodeWageringResponse(t, secondBody)
	if result.Status != "REJECTED" || result.FailureCode != "REFERENCE_KIND_NOT_REVERSIBLE" {
		t.Fatalf("result = %+v, want REJECTED/REFERENCE_KIND_NOT_REVERSIBLE", result)
	}
}

func TestWageringRollback_OfLoss_ReferenceKindNotReversible(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	lossExternalID := uniqueID("ext")
	loss, lossBody := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: lossExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: round, GameID: "game-1", Kind: "LOSS", Amount: "0.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if loss.StatusCode != http.StatusOK {
		t.Fatalf("LOSS status = %d, want 200, body = %s", loss.StatusCode, lossBody)
	}

	resp, body := doReversal(t, h, token, wallet, "ROLLBACK", round, "10.00", lossExternalID)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "REJECTED" || result.FailureCode != "REFERENCE_KIND_NOT_REVERSIBLE" {
		t.Fatalf("result = %+v, want REJECTED/REFERENCE_KIND_NOT_REVERSIBLE", result)
	}
}

// TestWageringRollback_ExceedsBalance_ReversalInsufficientFunds proves
// REVERSAL_INSUFFICIENT_FUNDS is a distinct, auditable code from
// INSUFFICIENT_FUNDS: the WIN itself succeeds against a small wallet, the
// player then spends the credit elsewhere (another BET), and rolling the
// WIN back would take the balance negative.
func TestWageringRollback_ExceedsBalance_ReversalInsufficientFunds(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "10.00")
	token := providerAToken(t)
	round := uniqueID("round")

	winExternalID, winResult := mustWinHTTP(t, h, token, wallet, round, "25.00")
	if winResult.Balance.Amount != "35.00" {
		t.Fatalf("win balance = %s, want 35.00", winResult.Balance.Amount)
	}

	betResp, betBody := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: round, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("spending BET status = %d, want 200, body = %s", betResp.StatusCode, betBody)
	}

	resp, body := doReversal(t, h, token, wallet, "ROLLBACK", round, "25.00", winExternalID)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "REJECTED" || result.FailureCode != "REVERSAL_INSUFFICIENT_FUNDS" {
		t.Fatalf("result = %+v, want REJECTED/REVERSAL_INSUFFICIENT_FUNDS (distinct from INSUFFICIENT_FUNDS)", result)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 500 {
		t.Fatalf("stored balance = %d, want 500 (unchanged by the rejected rollback)", balance)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

func TestWageringRefund_AmountMismatch_ReferenceAmountMismatch(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	betExternalID, _ := mustBetHTTP(t, h, token, wallet, round, "30.00")

	resp, body := doReversal(t, h, token, wallet, "REFUND", round, "20.00", betExternalID)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "REJECTED" || result.FailureCode != "REFERENCE_AMOUNT_MISMATCH" {
		t.Fatalf("result = %+v, want REJECTED/REFERENCE_AMOUNT_MISMATCH", result)
	}
}

// TestWageringRefund_RejectedReference_ReferenceNotProcessed refunds a BET
// that itself was REJECTED for insufficient funds - nothing to reverse.
func TestWageringRefund_RejectedReference_ReferenceNotProcessed(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "10.00")
	token := providerAToken(t)
	round := uniqueID("round")

	betExternalID := uniqueID("ext")
	bet, betBody := doWagering(t, h, token, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: betExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: round, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}, "idem-"+uniqueID("k"), "")
	if bet.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("setup BET status = %d, want 422 (insufficient funds), body = %s", bet.StatusCode, betBody)
	}
	betResult := decodeWageringResponse(t, betBody)
	if betResult.FailureCode != "INSUFFICIENT_FUNDS" {
		t.Fatalf("setup BET failureCode = %q, want INSUFFICIENT_FUNDS", betResult.FailureCode)
	}

	resp, body := doReversal(t, h, token, wallet, "REFUND", round, "30.00", betExternalID)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "REJECTED" || result.FailureCode != "REFERENCE_NOT_PROCESSED" {
		t.Fatalf("result = %+v, want REJECTED/REFERENCE_NOT_PROCESSED", result)
	}
}

// TestWageringReversal_MissingReference_NotImplemented is this ticket's own
// documented boundary: a REFUND (or ROLLBACK, or referenced WIN) whose
// reference has never arrived answers a plain, explicit "not implemented"
// error rather than silently waiting or guessing - ticket 11 replaces this
// with a persisted PENDING_REFERENCE and a retry worker.
func TestWageringReversal_MissingReference_NotImplemented(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	var before int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`).Scan(&before), "count external transactions before")

	resp, body := doReversal(t, h, token, wallet, "REFUND", round, "30.00", uniqueID("never-arrives"))
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501, body = %s", resp.StatusCode, body)
	}
	errResp := decodeErrorResponse(t, body)
	if errResp.Error.Code != "REFERENCE_RESOLUTION_NOT_IMPLEMENTED" {
		t.Errorf("error code = %q, want REFERENCE_RESOLUTION_NOT_IMPLEMENTED", errResp.Error.Code)
	}

	var after int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`).Scan(&after), "count external transactions after")
	if after != before {
		t.Errorf("external transactions count changed from %d to %d - an unresolved reference must persist nothing", before, after)
	}
}

// TestWageringReversal_ConcurrentRefundAndRollback_ExactlyOneCredit sends a
// REFUND and a ROLLBACK of the same BET in parallel: both lock the same
// wallet row, so one commits first and the other resolves the reference
// only after that commit is visible, finding it already reversed. Exactly
// one of the two ever credits the wallet.
func TestWageringReversal_ConcurrentRefundAndRollback_ExactlyOneCredit(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	betExternalID, _ := mustBetHTTP(t, h, token, wallet, round, "30.00")

	statusCodes := make([]int, 2)
	results := make([]wageringHTTPResponse, 2)
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})
	ready.Add(2)
	kinds := [2]string{"REFUND", "ROLLBACK"}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			resp, body := doReversal(t, h, token, wallet, kinds[i], round, "30.00", betExternalID)
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
			if results[i].FailureCode != "REFERENCE_ALREADY_REVERSED" {
				t.Errorf("attempt %d: 422 but failureCode = %q, want REFERENCE_ALREADY_REVERSED", i, results[i].FailureCode)
			}
		default:
			t.Errorf("attempt %d: unexpected status code %d", i, code)
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("processed = %d, rejected = %d, want exactly 1 and 1", processed, rejected)
	}

	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 10000 {
		t.Fatalf("stored balance = %d, want 10000 (exactly one credit of 30.00 on top of the 70.00 left by the bet)", balance)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}
