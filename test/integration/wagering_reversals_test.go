//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
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

func TestWageringReversal_MissingReference_IsAcceptedPending(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")

	var before int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`).Scan(&before), "count external transactions before")

	resp, body := doReversal(t, h, token, wallet, "REFUND", round, "30.00", uniqueID("never-arrives"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode, body)
	}
	result := decodeWageringResponse(t, body)
	if result.Status != "PENDING_REFERENCE" || result.FailureCode != "" {
		t.Errorf("result = %+v, want PENDING_REFERENCE without failure", result)
	}

	var after int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`).Scan(&after), "count external transactions after")
	if after != before+1 {
		t.Errorf("external transactions count = %d, want %d after persisting pending operation", after, before+1)
	}
}

func TestWageringRefund_PendingBeforeBet_IsCompletedByWorker(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	round := uniqueID("round")
	betExternalID := uniqueID("bet")

	resp, body := doReversal(t, h, token, wallet, "REFUND", round, "30.00", betExternalID)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pending refund status = %d, want 202, body = %s", resp.StatusCode, body)
	}
	pending := decodeWageringResponse(t, body)

	betResp, betBody := doWagering(t, h, token, wageringBodyInput{ProviderID: "provider-a", ExternalID: betExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}, "idem-"+uniqueID("k"), "")
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("BET status = %d, want 200, body = %s", betResp.StatusCode, betBody)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		requireNoError(t, h.pool.QueryRow(ctx, `SELECT status FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&status), "read pending refund status")
		if status == "PROCESSED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending refund status = %s after worker deadline, want PROCESSED", status)
		}
		time.Sleep(25 * time.Millisecond)
	}
	balance, _, _ := queryWalletRow(t, ctx, h, wallet.ID)
	if balance != 10000 {
		t.Errorf("wallet balance = %d, want 10000 after BET then REFUND", balance)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("ledger net = %d, want %d", net, balance)
	}
}

func TestWageringRefund_PendingReferenceExpiresRejected(t *testing.T) {
	t.Setenv("REFERENCE_WORKER_POLL_INTERVAL", "20ms")
	t.Setenv("REFERENCE_WORKER_RETRY_BASE", "20ms")
	t.Setenv("REFERENCE_WORKER_RETRY_MAX", "20ms")
	t.Setenv("REFERENCE_WORKER_TTL", "100ms")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	resp, body := doReversal(t, h, providerAToken(t), wallet, "REFUND", uniqueID("round"), "30.00", uniqueID("missing"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pending refund status = %d, want 202, body = %s", resp.StatusCode, body)
	}
	pending := decodeWageringResponse(t, body)
	deadline := time.Now().Add(5 * time.Second)
	var status string
	var failureCode *string
	for time.Now().Before(deadline) {
		requireNoError(t, h.pool.QueryRow(ctx, `SELECT status, failure_code FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&status, &failureCode), "read expired pending refund")
		if status == "REJECTED" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if status != "REJECTED" || failureCode == nil || *failureCode != "REFERENCE_NOT_FOUND" {
		t.Fatalf("expired pending = %s/%v, want REJECTED/REFERENCE_NOT_FOUND", status, failureCode)
	}
	events := queryOutboxEvents(t, ctx, h, pending.TransactionID)
	if len(events) != 2 || events[0].eventType != "WagerTransactionPendingReference" || events[1].eventType != "WagerTransactionRejected" {
		t.Errorf("outbox events = %+v, want pending then rejected", events)
	}
}

// seam 3a: two full Fx compositions share the same pending row. Their
// independent workers may race, but exactly one terminal transition and one
// financial effect are observable after the referenced BET arrives.
func TestPendingReference_TwoWorkersCompleteItExactlyOnce(t *testing.T) {
	t.Setenv("REFERENCE_WORKER_POLL_INTERVAL", "20ms")
	h1 := newAppHarness(t)
	h2 := newAppHarness(t)
	_ = h2 // Its independently composed worker races h1's worker through PostgreSQL.
	ctx := context.Background()
	wallet := openWalletHTTP(t, h1, "100.00")
	round := uniqueID("round")
	betExternalID := uniqueID("bet")

	resp, body := doReversal(t, h1, providerAToken(t), wallet, "REFUND", round, "30.00", betExternalID)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pending REFUND status = %d, want 202, body = %s", resp.StatusCode, body)
	}
	pending := decodeWageringResponse(t, body)
	betResp, betBody := doWagering(t, h1, providerAToken(t), wageringBodyInput{ProviderID: "provider-a", ExternalID: betExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}, "idem-"+uniqueID("key"), "")
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("BET status = %d, want 200, body = %s", betResp.StatusCode, betBody)
	}

	deadline := time.Now().Add(5 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		requireNoError(t, h1.pool.QueryRow(ctx, `SELECT status FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&status), "read contested pending status")
		if status == "PROCESSED" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if status != "PROCESSED" {
		t.Fatalf("contested pending status = %s, want PROCESSED", status)
	}
	if balance, _, found := queryWalletRow(t, ctx, h1, wallet.ID); !found || balance != 10000 {
		t.Errorf("wallet balance = %d (found %v), want 10000", balance, found)
	}
	if entries := countLedgerEntries(t, ctx, h1, wallet.ID); entries != 3 {
		t.Errorf("ledger entries = %d, want 3 (OPENING, BET, REFUND)", entries)
	}
	events := queryOutboxEvents(t, ctx, h1, pending.TransactionID)
	if len(events) != 2 || events[0].eventType != "WagerTransactionPendingReference" || events[1].eventType != "WagerTransactionProcessed" {
		t.Errorf("pending transaction events = %+v, want one pending then one processed", events)
	}
}

func TestWageringRefund_PendingReferenceExhaustsAttemptsRejected(t *testing.T) {
	t.Setenv("REFERENCE_WORKER_POLL_INTERVAL", "20ms")
	t.Setenv("REFERENCE_WORKER_RETRY_BASE", "20ms")
	t.Setenv("REFERENCE_WORKER_RETRY_MAX", "20ms")
	t.Setenv("REFERENCE_WORKER_MAX_ATTEMPTS", "1")
	t.Setenv("REFERENCE_WORKER_TTL", "1h")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	resp, body := doReversal(t, h, providerAToken(t), wallet, "REFUND", uniqueID("round"), "30.00", uniqueID("missing"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pending refund status = %d, want 202, body = %s", resp.StatusCode, body)
	}
	pending := decodeWageringResponse(t, body)
	deadline := time.Now().Add(5 * time.Second)
	var status, failureCode string
	for time.Now().Before(deadline) {
		requireNoError(t, h.pool.QueryRow(ctx, `SELECT status, COALESCE(failure_code, '') FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&status, &failureCode), "read attempt-exhausted pending refund")
		if status == "REJECTED" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if status != "REJECTED" || failureCode != "REFERENCE_NOT_FOUND" {
		t.Fatalf("attempt-exhausted pending = %s/%s, want REJECTED/REFERENCE_NOT_FOUND", status, failureCode)
	}
}

func TestPendingReferenceAtExhaustionStillProcessesArrivedBet(t *testing.T) {
	for _, tt := range []struct {
		name       string
		exhaustSQL string
	}{
		{name: "attempt limit", exhaustSQL: `UPDATE wager_transactions SET attempts = 1, next_attempt_at = now() WHERE id = $1`},
		{name: "ttl", exhaustSQL: `UPDATE wager_transactions SET pending_expires_at = now() - interval '1 second', next_attempt_at = now() WHERE id = $1`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("REFERENCE_WORKER_ENABLED", "false")
			t.Setenv("REFERENCE_WORKER_MAX_ATTEMPTS", "1")
			h := newAppHarness(t)
			ctx := context.Background()
			wallet := openWalletHTTP(t, h, "100.00")
			round, betExternalID := uniqueID("round"), uniqueID("bet")
			response, body := doReversal(t, h, providerAToken(t), wallet, "REFUND", round, "30.00", betExternalID)
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("pending REFUND status = %d, want 202, body = %s", response.StatusCode, body)
			}
			pending := decodeWageringResponse(t, body)
			if pending.PendingExpiresAt == nil {
				t.Fatal("202 pendingExpiresAt = nil, want stored PostgreSQL value")
			}
			var storedExpiry time.Time
			requireNoError(t, h.pool.QueryRow(ctx, `SELECT pending_expires_at FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&storedExpiry), "read persisted pending expiry")
			if !storedExpiry.UTC().Truncate(time.Second).Equal(pending.PendingExpiresAt.UTC().Truncate(time.Second)) {
				t.Fatalf("202 pendingExpiresAt = %s, want stored PostgreSQL value %s", pending.PendingExpiresAt, storedExpiry)
			}
			_, err := h.pool.Exec(ctx, tt.exhaustSQL, pending.TransactionID)
			requireNoError(t, err, "exhaust pending before resumption")

			betResponse, betBody := doWagering(t, h, providerAToken(t), wageringBodyInput{ProviderID: "provider-a", ExternalID: betExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}, "idem-"+uniqueID("key"), "")
			if betResponse.StatusCode != http.StatusOK {
				t.Fatalf("BET status = %d, want 200, body = %s", betResponse.StatusCode, betBody)
			}
			if err := h.worker.ProcessBatch(ctx); err != nil {
				t.Fatalf("ProcessBatch() error = %v", err)
			}

			var status string
			requireNoError(t, h.pool.QueryRow(ctx, `SELECT status FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&status), "read resumed pending")
			if status != "PROCESSED" {
				t.Fatalf("pending status = %s, want PROCESSED", status)
			}
			if balance, _, found := queryWalletRow(t, ctx, h, wallet.ID); !found || balance != 10000 {
				t.Errorf("wallet balance = %d (found %v), want 10000", balance, found)
			}
			if entries := countLedgerEntries(t, ctx, h, wallet.ID); entries != 3 {
				t.Errorf("ledger entries = %d, want 3", entries)
			}
			events := queryOutboxEvents(t, ctx, h, pending.TransactionID)
			if len(events) != 2 {
				t.Fatalf("pending events = %+v, want pending and processed", events)
			}
			var event eventEnvelopeJSON
			requireNoError(t, json.Unmarshal(events[1].payload, &event), "decode worker event")
			if event.CorrelationID != pending.TransactionID || event.CausationID != pending.TransactionID {
				t.Errorf("worker event correlation/causation = %q/%q, want %q/%q", event.CorrelationID, event.CausationID, pending.TransactionID, pending.TransactionID)
			}
		})
	}
}

func TestWageringPendingReferenceKindsBeforeBetCompleteByHTTP(t *testing.T) {
	for _, kind := range []string{"REFUND", "ROLLBACK", "WIN"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("REFERENCE_WORKER_POLL_INTERVAL", "20ms")
			h := newAppHarness(t)
			ctx := context.Background()
			wallet := openWalletHTTP(t, h, "100.00")
			round, betExternalID := uniqueID("round"), uniqueID("bet")
			resp, body := doReversal(t, h, providerAToken(t), wallet, kind, round, "30.00", betExternalID)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("pending %s status = %d, want 202, body = %s", kind, resp.StatusCode, body)
			}
			pending := decodeWageringResponse(t, body)
			betResp, betBody := doWagering(t, h, providerAToken(t), wageringBodyInput{ProviderID: "provider-a", ExternalID: betExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}, "idem-"+uniqueID("key"), "")
			if betResp.StatusCode != http.StatusOK {
				t.Fatalf("BET status = %d, want 200, body = %s", betResp.StatusCode, betBody)
			}
			deadline := time.Now().Add(5 * time.Second)
			var status string
			for time.Now().Before(deadline) {
				requireNoError(t, h.pool.QueryRow(ctx, `SELECT status FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&status), "read pending operation")
				if status == "PROCESSED" {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if status != "PROCESSED" {
				t.Fatalf("pending %s status = %s, want PROCESSED", kind, status)
			}
			if balance, _, found := queryWalletRow(t, ctx, h, wallet.ID); !found || balance != 10000 {
				t.Errorf("%s wallet balance = %d (found %v), want 10000", kind, balance, found)
			}
		})
	}
}

func TestWageringPendingReference_RejectedReferenceIsRejectedNotProcessed(t *testing.T) {
	t.Setenv("REFERENCE_WORKER_POLL_INTERVAL", "20ms")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	round, betExternalID := uniqueID("round"), uniqueID("bet")
	resp, body := doReversal(t, h, providerAToken(t), wallet, "REFUND", round, "200.00", betExternalID)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pending REFUND status = %d, want 202, body = %s", resp.StatusCode, body)
	}
	pending := decodeWageringResponse(t, body)
	betResp, betBody := doWagering(t, h, providerAToken(t), wageringBodyInput{ProviderID: "provider-a", ExternalID: betExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "BET", Amount: "200.00", Currency: testCurrency}, "idem-"+uniqueID("key"), "")
	if betResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("rejected BET status = %d, want 422, body = %s", betResp.StatusCode, betBody)
	}
	deadline := time.Now().Add(5 * time.Second)
	var status, failureCode string
	for time.Now().Before(deadline) {
		requireNoError(t, h.pool.QueryRow(ctx, `SELECT status, COALESCE(failure_code, '') FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&status, &failureCode), "read pending rejection")
		if status == "REJECTED" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if status != "REJECTED" || failureCode != "REFERENCE_NOT_PROCESSED" {
		t.Fatalf("pending after rejected reference = %s/%s, want REJECTED/REFERENCE_NOT_PROCESSED", status, failureCode)
	}
}

// TestReferenceWorker_StopCompletesFxComposition proves Stop abandons an
// in-flight resume with a rollback: the pending reference must still be
// PENDING_REFERENCE afterward, untouched, and become claimable again once
// its own lease elapses, not after a retry backoff. Ordering used to be a
// timer race - a worker polling on its own interval against this test's HTTP
// round trips and lock acquisition - which stayed a race no matter how much
// slack was added between the interval and the deadline. Instead, h1 starts
// with its reference worker disabled (REFERENCE_WORKER_ENABLED=false), so
// nothing claims anything while the REFUND is submitted and the wallet lock
// is taken; only once both are done does h2 - a second instance of the whole
// Fx app - start with the worker enabled. Worker.start claims immediately
// (Worker.run calls ProcessBatch before its own poll interval's first tick),
// so h2's very first claim attempt is ordered after the REFUND and the lock
// by process start order alone, never by wall-clock chance.
func TestReferenceWorker_StopCompletesFxComposition(t *testing.T) {
	t.Setenv("REFERENCE_WORKER_ENABLED", "false")
	h1 := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h1, "100.00")
	round, betExternalID := uniqueID("round"), uniqueID("bet")
	response, body := doReversal(t, h1, providerAToken(t), wallet, "REFUND", round, "30.00", betExternalID)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("pending REFUND status = %d, want 202, body = %s", response.StatusCode, body)
	}
	pending := decodeWageringResponse(t, body)
	lock := connectApp(t, ctx)
	lockTx, err := lock.Begin(ctx)
	requireNoError(t, err, "begin controlled wallet lock")
	requireNoError(t, lockTx.QueryRow(ctx, `SELECT id FROM wallets WHERE id = $1 FOR UPDATE`, wallet.ID).Scan(new(string)), "lock wallet while worker resumes")

	t.Setenv("REFERENCE_WORKER_ENABLED", "true")
	t.Setenv("REFERENCE_WORKER_LEASE", "150ms")
	h2 := newAppHarness(t)

	deadline := time.Now().Add(2 * time.Second)
	claimed := false
	for time.Now().Before(deadline) {
		requireNoError(t, h2.pool.QueryRow(ctx, `SELECT next_attempt_at > now() FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&claimed), "observe worker claim")
		if claimed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !claimed {
		t.Fatal("worker did not claim the pending operation while its wallet lock was held")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h2.stop(t, stopCtx); err != nil {
		t.Fatalf("Fx stop with reference worker polling = %v", err)
	}
	requireNoError(t, lockTx.Rollback(context.Background()), "release controlled wallet lock")
	var status string
	requireNoError(t, lock.QueryRow(ctx, `SELECT status FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&status), "read stopped pending")
	if status != "PENDING_REFERENCE" {
		t.Errorf("pending status after stop = %s, want PENDING_REFERENCE", status)
	}
	var entries int
	requireNoError(t, lock.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, wallet.ID).Scan(&entries), "count ledger after stop")
	if entries != 1 {
		t.Errorf("ledger entries after stop = %d, want only opening", entries)
	}
	var balance int64
	requireNoError(t, lock.QueryRow(ctx, `SELECT balance FROM wallets WHERE id = $1`, wallet.ID).Scan(&balance), "read wallet after stop")
	if balance != 10000 {
		t.Errorf("wallet balance after stop = %d, want 10000", balance)
	}
	time.Sleep(200 * time.Millisecond)
	var eligible bool
	requireNoError(t, lock.QueryRow(ctx, `SELECT next_attempt_at <= now() FROM wager_transactions WHERE id = $1`, pending.TransactionID).Scan(&eligible), "read pending eligibility after lease")
	if !eligible {
		t.Error("pending did not become eligible after its lease")
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
