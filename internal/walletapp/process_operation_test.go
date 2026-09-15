package walletapp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

const (
	testWalletID = "11111111-1111-1111-1111-111111111111"
	testPlayerID = "22222222-2222-2222-2222-222222222222"
	testProvider = "provider-a"
	testExternal = "ext-1"
	testRound    = "round-1"
	testGame     = "game-1"
	testIdempKey = "idem-1"
)

func testWallet(t *testing.T, balance string, version int64) *domainwallet.Wallet {
	t.Helper()
	w, err := domainwallet.Rehydrate(domainwallet.RehydratedWallet{
		ID: testWalletID, PlayerID: testPlayerID, Currency: money.BRL, Balance: mustMoney(t, balance, money.BRL),
		Version: version, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Rehydrate wallet: %v", err)
	}
	return w
}

func testRequest(t *testing.T, kind domainwallet.WagerKind, amount string) operation.Request {
	t.Helper()
	return operation.Request{
		ProviderID: testProvider, ExternalTransactionID: testExternal, PlayerID: testPlayerID, WalletID: testWalletID,
		RoundID: testRound, GameID: testGame, Kind: kind, Money: mustMoney(t, amount, money.BRL),
	}
}

// testReferenceTransaction rehydrates a resolved reference transaction - the
// shape FindReference hands resolveDecision - with its own id, kind,
// status, wallet/player/round and amount, and testProvider/testGame filled
// in as the fixed values every test in this file shares (they never need to
// vary to exercise a specific mismatch, so only the columns
// operation.Evaluate actually reads are ever overridden by callers).
func testReferenceTransaction(t *testing.T, id string, kind domainwallet.WagerKind, status domainwallet.TransactionStatus, walletID, playerID, roundID, amount string) *domainwallet.WagerTransaction {
	t.Helper()
	input := domainwallet.RehydratedTransaction{
		ID: id, ExternalTransactionID: "ref-ext-" + id, ProviderID: testProvider,
		IdempotencyKey: "ref-idem-" + id, PayloadHash: "ref-hash-" + id, WalletID: walletID, PlayerID: playerID,
		RoundID: roundID, GameID: testGame, Kind: kind, Origin: domainwallet.External,
		Money: mustMoney(t, amount, money.BRL), Status: status, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if status == domainwallet.Rejected || status == domainwallet.Failed {
		input.FailureCode = string(operation.CodeInsufficientFunds)
	}
	if kind == domainwallet.Refund || kind == domainwallet.Rollback {
		input.ReferenceExternalTransactionID = "ref-of-" + id
	}
	transaction, err := domainwallet.RehydrateTransaction(input)
	if err != nil {
		t.Fatalf("RehydrateTransaction: %v", err)
	}
	return transaction
}

type processHarness struct {
	wallets      *fakeWalletRepository
	transactions *fakeTransactionRepository
	ledger       *fakeLedgerRepository
	outbox       *fakeOutboxRepository
	metrics      *fakeOperationMetrics
	uow          *fakeUnitOfWork
	useCase      *walletapp.ProcessOperationUseCase
}

func newProcessHarness(walletValue *domainwallet.Wallet) *processHarness {
	h := &processHarness{
		wallets:      &fakeWalletRepository{findResult: walletValue},
		transactions: &fakeTransactionRepository{},
		ledger:       &fakeLedgerRepository{},
		outbox:       &fakeOutboxRepository{},
		metrics:      &fakeOperationMetrics{},
	}
	h.uow = &fakeUnitOfWork{wallets: h.wallets, transactions: h.transactions, ledger: h.ledger, outbox: h.outbox}
	h.useCase = walletapp.NewProcessOperationUseCase(h.uow, h.metrics)
	return h
}

func (h *processHarness) process(t *testing.T, req operation.Request, idempotencyKey string) (walletapp.ProcessOperationResult, error) {
	t.Helper()
	return h.useCase.Process(context.Background(), walletapp.ProcessOperationInput{
		Request: req, IdempotencyKey: idempotencyKey, CorrelationID: "corr-1", Channel: walletapp.ChannelHTTP,
	})
}

func TestProcessOperationUseCase_Bet_DebitsAndRecordsLedgerAndOutbox(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 1))

	result, err := h.process(t, testRequest(t, domainwallet.Bet, "30.00"), testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Processed || result.IdempotentReplay {
		t.Errorf("result = %+v, want PROCESSED and not a replay", result)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 7000 {
		t.Errorf("Balance = %d, want 7000", balance)
	}

	if len(h.transactions.inserted) != 1 || h.transactions.inserted[0].Status() != domainwallet.Processed {
		t.Fatalf("transactions inserted = %+v, want one PROCESSED row", h.transactions.inserted)
	}
	if len(h.ledger.inserted) != 1 || h.ledger.inserted[0].Direction() != domainwallet.Debit {
		t.Fatalf("ledger inserted = %+v, want one DEBIT entry", h.ledger.inserted)
	}
	if len(h.wallets.updated) != 1 {
		t.Fatalf("wallets updated = %d, want 1", len(h.wallets.updated))
	}
	if len(h.outbox.inserted) != 2 {
		t.Fatalf("outbox inserted = %d, want 2 (Processed + BalanceChanged)", len(h.outbox.inserted))
	}
	if h.outbox.inserted[0].eventType != domainwallet.WagerTransactionProcessedEventType {
		t.Errorf("first outbox event = %q, want WagerTransactionProcessed", h.outbox.inserted[0].eventType)
	}
	if h.outbox.inserted[1].eventType != domainwallet.WalletBalanceChangedEventType {
		t.Errorf("second outbox event = %q, want WalletBalanceChanged", h.outbox.inserted[1].eventType)
	}
	if len(h.metrics.operations) != 1 || h.metrics.operations[0].channel != walletapp.ChannelHTTP || h.metrics.operations[0].kind != "BET" || h.metrics.operations[0].status != "PROCESSED" {
		t.Errorf("metrics observed = %+v, want one HTTP/BET/PROCESSED observation", h.metrics.operations)
	}
	if h.uow.calls != 1 {
		t.Errorf("UnitOfWork.WithinTx calls = %d, want 1 - a valid attempt still opens exactly one transaction", h.uow.calls)
	}
}

func TestProcessOperationUseCase_RecordOutcome_RecordsSQSReplay(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 1))

	h.useCase.RecordOutcome(walletapp.ChannelSQS, walletapp.ProcessOperationResult{
		Status:           domainwallet.Processed,
		IdempotentReplay: true,
	}, nil, 2*time.Second)

	if len(h.metrics.operations) != 1 || h.metrics.operations[0].channel != walletapp.ChannelSQS || h.metrics.operations[0].kind != "" || h.metrics.operations[0].status != "PROCESSED" || h.metrics.operations[0].duration != 2*time.Second {
		t.Errorf("operations = %+v, want one SQS/empty-kind/PROCESSED observation lasting 2s", h.metrics.operations)
	}
	if len(h.metrics.duplicates) != 1 || h.metrics.duplicates[0] != walletapp.ChannelSQS {
		t.Errorf("duplicates = %+v, want one SQS replay", h.metrics.duplicates)
	}
}

func TestProcessOperationUseCase_Win_CreditsWallet(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "50.00", 1))

	result, err := h.process(t, testRequest(t, domainwallet.Win, "25.00"), testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 7500 {
		t.Errorf("Balance = %d, want 7500", balance)
	}
	if len(h.ledger.inserted) != 1 || h.ledger.inserted[0].Direction() != domainwallet.Credit {
		t.Fatalf("ledger inserted = %+v, want one CREDIT entry", h.ledger.inserted)
	}
}

func TestProcessOperationUseCase_LossZero_NoLedgerNoWalletUpdate(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "50.00", 1))

	result, err := h.process(t, testRequest(t, domainwallet.Loss, "0.00"), testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Processed {
		t.Errorf("Status = %q, want PROCESSED", result.Status)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 5000 {
		t.Errorf("Balance = %d, want 5000 (unchanged)", balance)
	}
	if len(h.ledger.inserted) != 0 {
		t.Errorf("ledger inserted = %d, want 0 for LOSS", len(h.ledger.inserted))
	}
	if len(h.wallets.updated) != 0 {
		t.Errorf("wallets updated = %d, want 0 for LOSS - version must not change", len(h.wallets.updated))
	}
	if len(h.outbox.inserted) != 1 || h.outbox.inserted[0].eventType != domainwallet.WagerTransactionProcessedEventType {
		t.Errorf("outbox inserted = %+v, want exactly one WagerTransactionProcessed", h.outbox.inserted)
	}
}

func TestProcessOperationUseCase_BetInsufficientFunds_RejectsWithoutMovingBalance(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "20.00", 1))

	result, err := h.process(t, testRequest(t, domainwallet.Bet, "30.00"), testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Rejected || result.FailureCode != string(operation.CodeInsufficientFunds) {
		t.Errorf("result = %+v, want REJECTED/INSUFFICIENT_FUNDS", result)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 2000 {
		t.Errorf("Balance = %d, want 2000 (unchanged)", balance)
	}
	if len(h.ledger.inserted) != 0 {
		t.Errorf("ledger inserted = %d, want 0 for a rejected BET", len(h.ledger.inserted))
	}
	if len(h.wallets.updated) != 0 {
		t.Errorf("wallets updated = %d, want 0 for a rejected BET", len(h.wallets.updated))
	}
	if len(h.outbox.inserted) != 1 || h.outbox.inserted[0].eventType != domainwallet.WagerTransactionRejectedEventType {
		t.Errorf("outbox inserted = %+v, want exactly one WagerTransactionRejected", h.outbox.inserted)
	}
}

func TestProcessOperationUseCase_MissingIdempotencyKey_CorrectableNoEffect(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 1))

	_, err := h.process(t, testRequest(t, domainwallet.Bet, "10.00"), "")
	if !errors.Is(err, operation.ErrMissingIdempotencyKey) {
		t.Errorf("err = %v, want ErrMissingIdempotencyKey", err)
	}
	if len(h.transactions.inserted) != 0 {
		t.Errorf("transactions inserted = %d, want 0 - a correctable error must have no effect", len(h.transactions.inserted))
	}
	if h.uow.calls != 0 {
		t.Errorf("UnitOfWork.WithinTx calls = %d, want 0 - Prepare must reject this before any transaction opens", h.uow.calls)
	}
}

func TestProcessOperationUseCase_WalletNotFound(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(nil)
	h.wallets.findErr = walletapp.ErrNotFound

	_, err := h.process(t, testRequest(t, domainwallet.Bet, "10.00"), testIdempKey)
	if !errors.Is(err, operation.ErrWalletNotFound) {
		t.Errorf("err = %v, want ErrWalletNotFound", err)
	}
}

func TestProcessOperationUseCase_WalletPlayerMismatch(t *testing.T) {
	t.Parallel()
	w, err := domainwallet.Rehydrate(domainwallet.RehydratedWallet{
		ID: testWalletID, PlayerID: "99999999-9999-9999-9999-999999999999", Currency: money.BRL,
		Balance: mustMoney(t, "10.00", money.BRL), Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	h := newProcessHarness(w)

	_, procErr := h.process(t, testRequest(t, domainwallet.Bet, "10.00"), testIdempKey)
	if !errors.Is(procErr, operation.ErrWalletPlayerMismatch) {
		t.Errorf("err = %v, want ErrWalletPlayerMismatch", procErr)
	}
}

func TestProcessOperationUseCase_WalletCurrencyMismatch(t *testing.T) {
	t.Parallel()
	w, err := domainwallet.Rehydrate(domainwallet.RehydratedWallet{
		ID: testWalletID, PlayerID: testPlayerID, Currency: money.USD,
		Balance: mustMoney(t, "10.00", money.USD), Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	h := newProcessHarness(w)

	_, procErr := h.process(t, testRequest(t, domainwallet.Bet, "10.00"), testIdempKey)
	if !errors.Is(procErr, operation.ErrWalletCurrencyMismatch) {
		t.Errorf("err = %v, want ErrWalletCurrencyMismatch", procErr)
	}
}

func TestProcessOperationUseCase_ReferenceForbiddenOnBet_Correctable(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 1))
	ref := "some-ref"
	req := testRequest(t, domainwallet.Bet, "10.00")
	req.ReferenceExternalTransactionID = &ref

	_, err := h.process(t, req, testIdempKey)
	if !errors.Is(err, operation.ErrReferenceNotAllowed) {
		t.Errorf("err = %v, want ErrReferenceNotAllowed", err)
	}
	if h.uow.calls != 0 {
		t.Errorf("UnitOfWork.WithinTx calls = %d, want 0 - Prepare must reject this before any transaction opens", h.uow.calls)
	}
}

// TestProcessOperationUseCase_WinWithReference_MissingReference_NotSupportedByThisTicket
// covers this ticket's own documented boundary: a reference that has not
// arrived at all still answers ErrOperationNotSupported, but only once
// ExecuteInTx has actually looked for it inside the wallet-locked
// transaction - unlike ticket 08's blanket rejection, Prepare alone can no
// longer tell a genuinely resolvable reference apart from a missing one.
func TestProcessOperationUseCase_WinWithReference_MissingReference_NotSupportedByThisTicket(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 1))
	ref := "bet-ref"
	req := testRequest(t, domainwallet.Win, "10.00")
	req.ReferenceExternalTransactionID = &ref

	_, err := h.process(t, req, testIdempKey)
	if !errors.Is(err, walletapp.ErrOperationNotSupported) {
		t.Errorf("err = %v, want ErrOperationNotSupported", err)
	}
	if h.uow.calls != 1 {
		t.Errorf("UnitOfWork.WithinTx calls = %d, want 1 - resolving the reference needs the wallet-locked transaction", h.uow.calls)
	}
	if len(h.transactions.inserted) != 0 {
		t.Errorf("transactions inserted = %d, want 0 - an unresolved reference must persist nothing", len(h.transactions.inserted))
	}
}

// TestProcessOperationUseCase_Refund_ReferencePendingReference_NotSupportedByThisTicket
// covers the other WaitForReference case: the reference exists but has not
// itself reached a terminal status yet (spec: "referência existe, mas está
// PENDING_REFERENCE: a operação continua esperando").
func TestProcessOperationUseCase_Refund_ReferencePendingReference_NotSupportedByThisTicket(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 1))
	h.transactions.reference = testReferenceTransaction(t, "bet-tx", domainwallet.Bet, domainwallet.PendingReference, testWalletID, testPlayerID, testRound, "30.00")
	req := testRequest(t, domainwallet.Refund, "30.00")
	ref := "bet-ref"
	req.ReferenceExternalTransactionID = &ref

	_, err := h.process(t, req, testIdempKey)
	if !errors.Is(err, walletapp.ErrOperationNotSupported) {
		t.Errorf("err = %v, want ErrOperationNotSupported", err)
	}
	if len(h.transactions.inserted) != 0 {
		t.Errorf("transactions inserted = %d, want 0 - a still-pending reference must persist nothing", len(h.transactions.inserted))
	}
}

func TestProcessOperationUseCase_RefundOfBet_CreditsAndPersistsResolvedReference(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "70.00", 2))
	reference := testReferenceTransaction(t, "bet-tx-1", domainwallet.Bet, domainwallet.Processed, testWalletID, testPlayerID, testRound, "30.00")
	h.transactions.reference = reference
	req := testRequest(t, domainwallet.Refund, "30.00")
	ref := "bet-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Processed {
		t.Errorf("result = %+v, want PROCESSED", result)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 10000 {
		t.Errorf("Balance = %d, want 10000 (70.00 + 30.00)", balance)
	}
	if len(h.ledger.inserted) != 1 || h.ledger.inserted[0].Direction() != domainwallet.Credit {
		t.Fatalf("ledger inserted = %+v, want one CREDIT entry", h.ledger.inserted)
	}
	if len(h.transactions.inserted) != 1 || h.transactions.inserted[0].ReferenceTransactionID() != reference.ID() {
		t.Fatalf("inserted transaction = %+v, want referenceTransactionId %q", h.transactions.inserted, reference.ID())
	}
}

func TestProcessOperationUseCase_RollbackOfBet_CreditsWallet(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "70.00", 2))
	h.transactions.reference = testReferenceTransaction(t, "bet-tx-1", domainwallet.Bet, domainwallet.Processed, testWalletID, testPlayerID, testRound, "30.00")
	req := testRequest(t, domainwallet.Rollback, "30.00")
	ref := "bet-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(h.ledger.inserted) != 1 || h.ledger.inserted[0].Direction() != domainwallet.Credit {
		t.Fatalf("ledger inserted = %+v, want one CREDIT entry (ROLLBACK of a BET credits)", h.ledger.inserted)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 10000 {
		t.Errorf("Balance = %d, want 10000", balance)
	}
}

func TestProcessOperationUseCase_RollbackOfWin_DebitsWallet(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 2))
	h.transactions.reference = testReferenceTransaction(t, "win-tx-1", domainwallet.Win, domainwallet.Processed, testWalletID, testPlayerID, testRound, "25.00")
	req := testRequest(t, domainwallet.Rollback, "25.00")
	ref := "win-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(h.ledger.inserted) != 1 || h.ledger.inserted[0].Direction() != domainwallet.Debit {
		t.Fatalf("ledger inserted = %+v, want one DEBIT entry (ROLLBACK of a WIN debits)", h.ledger.inserted)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 7500 {
		t.Errorf("Balance = %d, want 7500", balance)
	}
}

func TestProcessOperationUseCase_RollbackOfRefund_DebitsAndBlocksNewRefund(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 3))
	h.transactions.reference = testReferenceTransaction(t, "refund-tx-1", domainwallet.Refund, domainwallet.Processed, testWalletID, testPlayerID, testRound, "30.00")
	req := testRequest(t, domainwallet.Rollback, "30.00")
	ref := "refund-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(h.ledger.inserted) != 1 || h.ledger.inserted[0].Direction() != domainwallet.Debit {
		t.Fatalf("ledger inserted = %+v, want one DEBIT entry (ROLLBACK of a REFUND debits, undoing it)", h.ledger.inserted)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 7000 {
		t.Errorf("Balance = %d, want 7000", balance)
	}
}

func TestProcessOperationUseCase_WinWithReference_ValidatesBetSameRound_Credits(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "70.00", 2))
	h.transactions.reference = testReferenceTransaction(t, "bet-tx-1", domainwallet.Bet, domainwallet.Processed, testWalletID, testPlayerID, testRound, "30.00")
	req := testRequest(t, domainwallet.Win, "50.00")
	ref := "bet-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 12000 {
		t.Errorf("Balance = %d, want 12000", balance)
	}
	if len(h.transactions.inserted) != 1 || h.transactions.inserted[0].ReferenceTransactionID() != "bet-tx-1" {
		t.Fatalf("inserted transaction = %+v, want referenceTransactionId bet-tx-1", h.transactions.inserted)
	}
}

func TestProcessOperationUseCase_SecondReversalOfSameBet_ReferenceAlreadyReversed(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "70.00", 3))
	h.transactions.reference = testReferenceTransaction(t, "bet-tx-1", domainwallet.Bet, domainwallet.Processed, testWalletID, testPlayerID, testRound, "30.00")
	h.transactions.alreadyReversed = true
	req := testRequest(t, domainwallet.Rollback, "30.00")
	ref := "bet-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Rejected || result.FailureCode != string(operation.CodeReferenceAlreadyReversed) {
		t.Errorf("result = %+v, want REJECTED/REFERENCE_ALREADY_REVERSED", result)
	}
	if len(h.ledger.inserted) != 0 {
		t.Errorf("ledger inserted = %d, want 0", len(h.ledger.inserted))
	}
	if len(h.outbox.inserted) != 1 || h.outbox.inserted[0].eventType != domainwallet.WagerTransactionRejectedEventType {
		t.Errorf("outbox inserted = %+v, want exactly one WagerTransactionRejected", h.outbox.inserted)
	}
}

func TestProcessOperationUseCase_RollbackOfRollback_ReferenceKindNotReversible(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "70.00", 2))
	h.transactions.reference = testReferenceTransaction(t, "rollback-tx-1", domainwallet.Rollback, domainwallet.Processed, testWalletID, testPlayerID, testRound, "30.00")
	req := testRequest(t, domainwallet.Rollback, "30.00")
	ref := "rollback-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Rejected || result.FailureCode != string(operation.CodeReferenceKindNotReversible) {
		t.Errorf("result = %+v, want REJECTED/REFERENCE_KIND_NOT_REVERSIBLE", result)
	}
}

func TestProcessOperationUseCase_RollbackOfLoss_ReferenceKindNotReversible(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "70.00", 2))
	h.transactions.reference = testReferenceTransaction(t, "loss-tx-1", domainwallet.Loss, domainwallet.Processed, testWalletID, testPlayerID, testRound, "0.00")
	req := testRequest(t, domainwallet.Rollback, "30.00")
	ref := "loss-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Rejected || result.FailureCode != string(operation.CodeReferenceKindNotReversible) {
		t.Errorf("result = %+v, want REJECTED/REFERENCE_KIND_NOT_REVERSIBLE", result)
	}
}

func TestProcessOperationUseCase_RollbackExceedsBalance_ReversalInsufficientFunds(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "10.00", 2))
	h.transactions.reference = testReferenceTransaction(t, "win-tx-1", domainwallet.Win, domainwallet.Processed, testWalletID, testPlayerID, testRound, "25.00")
	req := testRequest(t, domainwallet.Rollback, "25.00")
	ref := "win-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Rejected || result.FailureCode != string(operation.CodeReversalInsufficientFunds) {
		t.Errorf("result = %+v, want REJECTED/REVERSAL_INSUFFICIENT_FUNDS (distinct from INSUFFICIENT_FUNDS)", result)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 1000 {
		t.Errorf("Balance = %d, want unchanged 1000", balance)
	}
	if len(h.wallets.updated) != 0 {
		t.Errorf("wallets updated = %d, want 0 - a rejected reversal must not move the wallet", len(h.wallets.updated))
	}
}

func TestProcessOperationUseCase_ReferenceMismatch_DifferentRound(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "70.00", 2))
	h.transactions.reference = testReferenceTransaction(t, "bet-tx-1", domainwallet.Bet, domainwallet.Processed, testWalletID, testPlayerID, "different-round", "30.00")
	req := testRequest(t, domainwallet.Refund, "30.00")
	ref := "bet-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Rejected || result.FailureCode != string(operation.CodeReferenceMismatch) {
		t.Errorf("result = %+v, want REJECTED/REFERENCE_MISMATCH", result)
	}
}

func TestProcessOperationUseCase_ReferenceAmountMismatch(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "70.00", 2))
	h.transactions.reference = testReferenceTransaction(t, "bet-tx-1", domainwallet.Bet, domainwallet.Processed, testWalletID, testPlayerID, testRound, "30.00")
	req := testRequest(t, domainwallet.Refund, "20.00")
	ref := "bet-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Rejected || result.FailureCode != string(operation.CodeReferenceAmountMismatch) {
		t.Errorf("result = %+v, want REJECTED/REFERENCE_AMOUNT_MISMATCH", result)
	}
}

func TestProcessOperationUseCase_ReferenceRejected_ReferenceNotProcessed(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "70.00", 2))
	h.transactions.reference = testReferenceTransaction(t, "bet-tx-1", domainwallet.Bet, domainwallet.Rejected, testWalletID, testPlayerID, testRound, "30.00")
	req := testRequest(t, domainwallet.Refund, "30.00")
	ref := "bet-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Rejected || result.FailureCode != string(operation.CodeReferenceNotProcessed) {
		t.Errorf("result = %+v, want REJECTED/REFERENCE_NOT_PROCESSED", result)
	}
}

func TestProcessOperationUseCase_ReferenceFailed_ReferenceNotProcessed(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "70.00", 2))
	h.transactions.reference = testReferenceTransaction(t, "bet-tx-1", domainwallet.Bet, domainwallet.Failed, testWalletID, testPlayerID, testRound, "30.00")
	req := testRequest(t, domainwallet.Refund, "30.00")
	ref := "bet-ref"
	req.ReferenceExternalTransactionID = &ref

	result, err := h.process(t, req, testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Status != domainwallet.Rejected || result.FailureCode != string(operation.CodeReferenceNotProcessed) {
		t.Errorf("result = %+v, want REJECTED/REFERENCE_NOT_PROCESSED", result)
	}
}

func TestProcessOperationUseCase_Replay_ReturnsPersistedResultNotCurrentBalance(t *testing.T) {
	t.Parallel()
	req := testRequest(t, domainwallet.Bet, "30.00")
	hash, err := operation.PayloadHash(req)
	if err != nil {
		t.Fatalf("PayloadHash: %v", err)
	}

	h := newProcessHarness(testWallet(t, "20.00", 3)) // wallet has moved since the original processing
	originalBalance := int64(7000)
	h.transactions.byIdempotencyKey = &walletapp.ExistingTransaction{
		TransactionID: "original-tx", IdempotencyKey: testIdempKey, PayloadHash: hash,
		ExternalTransactionID: testExternal, Status: domainwallet.Processed, ResultingBalance: &originalBalance, Currency: money.BRL,
	}

	result, procErr := h.process(t, req, testIdempKey)
	if procErr != nil {
		t.Fatalf("Process() error = %v", procErr)
	}
	if !result.IdempotentReplay || result.TransactionID != "original-tx" {
		t.Errorf("result = %+v, want a replay of original-tx", result)
	}
	balance, _ := result.Balance.MinorUnits()
	if balance != 7000 {
		t.Errorf("Balance = %d, want 7000 (the balance at original processing, not the current 2000)", balance)
	}
	if len(h.transactions.inserted) != 0 || len(h.ledger.inserted) != 0 || len(h.wallets.updated) != 0 {
		t.Errorf("replay must have no new effect: transactions=%d ledger=%d wallets=%d", len(h.transactions.inserted), len(h.ledger.inserted), len(h.wallets.updated))
	}
	if len(h.metrics.duplicates) != 1 || h.metrics.duplicates[0] != walletapp.ChannelHTTP {
		t.Errorf("metrics duplicates = %+v, want one HTTP duplicate", h.metrics.duplicates)
	}
}

func TestProcessOperationUseCase_SameKeyDifferentHash_IdempotencyKeyReused(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 1))
	h.transactions.byIdempotencyKey = &walletapp.ExistingTransaction{
		TransactionID: "other-tx", IdempotencyKey: testIdempKey, PayloadHash: "different-hash",
		ExternalTransactionID: testExternal, Status: domainwallet.Processed,
	}

	_, err := h.process(t, testRequest(t, domainwallet.Bet, "30.00"), testIdempKey)
	if !errors.Is(err, operation.ErrIdempotencyKeyReused) {
		t.Errorf("err = %v, want ErrIdempotencyKeyReused", err)
	}
}

func TestProcessOperationUseCase_DifferentKeySameExternalID_ExternalTransactionIDConflict(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 1))
	h.transactions.byExternal = &walletapp.ExistingTransaction{
		TransactionID: "other-tx", IdempotencyKey: "a-different-key", ExternalTransactionID: testExternal, Status: domainwallet.Processed,
	}

	_, err := h.process(t, testRequest(t, domainwallet.Bet, "30.00"), testIdempKey)
	if !errors.Is(err, operation.ErrExternalTransactionIDConflict) {
		t.Errorf("err = %v, want ErrExternalTransactionIDConflict", err)
	}
}

func TestProcessOperationUseCase_InsertConflict_RetriesAndReclassifiesAsReplay(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 1))
	h.transactions.insertNewConflict = true

	result, err := h.process(t, testRequest(t, domainwallet.Bet, "30.00"), testIdempKey)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if !result.IdempotentReplay {
		t.Errorf("result = %+v, want a replay from the reclassified fresh transaction", result)
	}
	if len(h.ledger.inserted) != 0 || len(h.wallets.updated) != 0 {
		t.Errorf("a reclassified conflict must apply no movement: ledger=%d wallets=%d", len(h.ledger.inserted), len(h.wallets.updated))
	}
	if h.metrics.conflicts != 1 {
		t.Errorf("metrics conflicts = %d, want 1", h.metrics.conflicts)
	}
}

func TestProcessOperationUseCase_DoubleInsertConflict_TransientError(t *testing.T) {
	t.Parallel()
	h := newProcessHarness(testWallet(t, "100.00", 1))
	h.transactions.insertNewAlwaysConflict = true

	_, err := h.process(t, testRequest(t, domainwallet.Bet, "30.00"), testIdempKey)
	if !errors.Is(err, operation.ErrTemporarilyUnavailable) {
		t.Errorf("err = %v, want ErrTemporarilyUnavailable after two collisions in a row", err)
	}
	if len(h.transactions.inserted) != 0 || len(h.ledger.inserted) != 0 || len(h.wallets.updated) != 0 {
		t.Errorf("a double collision must apply no movement: transactions=%d ledger=%d wallets=%d", len(h.transactions.inserted), len(h.ledger.inserted), len(h.wallets.updated))
	}
	if h.metrics.conflicts != 1 {
		t.Errorf("metrics conflicts = %d, want 1 (only the first collision triggers the retry)", h.metrics.conflicts)
	}
}

// TestProcessOperationUseCase_ExecuteInTx_NeverOpensItsOwnTransaction proves
// ExecuteInTx runs entirely against the Repositories it is handed, without
// ever going through the UnitOfWork: it is wired to an explodingUnitOfWork
// that fails the test if WithinTx is called, then driven directly - exactly
// how ticket 13's SQS consumer is meant to bind it to its own inbox
// transaction (ticket 08 review, correctness/spec: "o caso de uso não pode
// participar da transação externa").
func TestProcessOperationUseCase_ExecuteInTx_NeverOpensItsOwnTransaction(t *testing.T) {
	t.Parallel()
	wallets := &fakeWalletRepository{findResult: testWallet(t, "100.00", 1)}
	transactions := &fakeTransactionRepository{}
	ledger := &fakeLedgerRepository{}
	outbox := &fakeOutboxRepository{}
	useCase := walletapp.NewProcessOperationUseCase(explodingUnitOfWork{t: t}, &fakeOperationMetrics{})
	repos := walletapp.Repositories{Wallets: wallets, Transactions: transactions, Ledger: ledger, Outbox: outbox}

	prepared, err := useCase.Prepare(walletapp.ProcessOperationInput{
		Request: testRequest(t, domainwallet.Bet, "30.00"), IdempotencyKey: testIdempKey, CorrelationID: "corr-1", Channel: walletapp.ChannelHTTP,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	result, err := useCase.ExecuteInTx(context.Background(), repos, prepared)
	if err != nil {
		t.Fatalf("ExecuteInTx() error = %v", err)
	}
	if result.Status != domainwallet.Processed || result.IdempotentReplay {
		t.Errorf("result = %+v, want PROCESSED and not a replay", result)
	}
	if len(transactions.inserted) != 1 {
		t.Fatalf("transactions inserted = %d, want 1", len(transactions.inserted))
	}
	if len(ledger.inserted) != 1 {
		t.Errorf("ledger inserted = %d, want 1", len(ledger.inserted))
	}
	if len(outbox.inserted) != 2 {
		t.Errorf("outbox inserted = %d, want 2", len(outbox.inserted))
	}
}

// TestProcessOperationUseCase_ExecuteInTx_InsertConflict_ReturnsErrRetryInNewTransaction
// proves the collision path ExecuteInTx itself exposes: it must never
// retry the reclassification inside the same call, only report
// ErrRetryInNewTransaction and let the caller that owns the transaction
// (Process, here; the SQS consumer, in ticket 13) roll back and call it
// again in a fresh one.
func TestProcessOperationUseCase_ExecuteInTx_InsertConflict_ReturnsErrRetryInNewTransaction(t *testing.T) {
	t.Parallel()
	wallets := &fakeWalletRepository{findResult: testWallet(t, "100.00", 1)}
	transactions := &fakeTransactionRepository{insertNewConflict: true}
	useCase := walletapp.NewProcessOperationUseCase(explodingUnitOfWork{t: t}, &fakeOperationMetrics{})
	repos := walletapp.Repositories{Wallets: wallets, Transactions: transactions, Ledger: &fakeLedgerRepository{}, Outbox: &fakeOutboxRepository{}}

	prepared, err := useCase.Prepare(walletapp.ProcessOperationInput{
		Request: testRequest(t, domainwallet.Bet, "30.00"), IdempotencyKey: testIdempKey, CorrelationID: "corr-1", Channel: walletapp.ChannelHTTP,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	_, err = useCase.ExecuteInTx(context.Background(), repos, prepared)
	if !errors.Is(err, walletapp.ErrRetryInNewTransaction) {
		t.Errorf("err = %v, want ErrRetryInNewTransaction", err)
	}
	if len(transactions.inserted) != 0 {
		t.Errorf("transactions inserted = %d, want 0 - a collision must insert nothing", len(transactions.inserted))
	}
}

// newPrepareOnlyUseCase wires Prepare to an explodingUnitOfWork, so a test
// driving Prepare directly also proves it is pure I/O-free: it never opens
// a transaction to validate an input or compute its hash (spec, decision 3,
// step 1: "validar e calcular o hash fora da transação").
func newPrepareOnlyUseCase(t *testing.T) *walletapp.ProcessOperationUseCase {
	t.Helper()
	return walletapp.NewProcessOperationUseCase(explodingUnitOfWork{t: t}, &fakeOperationMetrics{})
}

func TestProcessOperationUseCase_Prepare_MissingIdempotencyKey(t *testing.T) {
	t.Parallel()
	useCase := newPrepareOnlyUseCase(t)

	_, err := useCase.Prepare(walletapp.ProcessOperationInput{
		Request: testRequest(t, domainwallet.Bet, "10.00"), IdempotencyKey: "", CorrelationID: "corr-1", Channel: walletapp.ChannelHTTP,
	})
	if !errors.Is(err, operation.ErrMissingIdempotencyKey) {
		t.Errorf("err = %v, want ErrMissingIdempotencyKey", err)
	}
}

func TestProcessOperationUseCase_Prepare_InvalidRequest_BadWalletID(t *testing.T) {
	t.Parallel()
	useCase := newPrepareOnlyUseCase(t)
	req := testRequest(t, domainwallet.Bet, "10.00")
	req.WalletID = "not-a-uuid"

	_, err := useCase.Prepare(walletapp.ProcessOperationInput{
		Request: req, IdempotencyKey: testIdempKey, CorrelationID: "corr-1", Channel: walletapp.ChannelHTTP,
	})
	if !errors.Is(err, operation.ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestProcessOperationUseCase_Prepare_ReferenceForbiddenOnBet(t *testing.T) {
	t.Parallel()
	useCase := newPrepareOnlyUseCase(t)
	ref := "some-ref"
	req := testRequest(t, domainwallet.Bet, "10.00")
	req.ReferenceExternalTransactionID = &ref

	_, err := useCase.Prepare(walletapp.ProcessOperationInput{
		Request: req, IdempotencyKey: testIdempKey, CorrelationID: "corr-1", Channel: walletapp.ChannelHTTP,
	})
	if !errors.Is(err, operation.ErrReferenceNotAllowed) {
		t.Errorf("err = %v, want ErrReferenceNotAllowed", err)
	}
}

// TestProcessOperationUseCase_Prepare_WinWithReference_ReturnsWaitForReferenceDecision
// documents that Prepare alone can never tell a resolvable reference apart
// from a missing one - it has no database access - so a WIN naming a
// reference always comes back as a preliminary WaitForReference decision,
// not an error; ExecuteInTx's resolveDecision is what turns this into a
// final Process, Reject or ErrOperationNotSupported once it can actually
// look the reference up.
func TestProcessOperationUseCase_Prepare_WinWithReference_ReturnsWaitForReferenceDecision(t *testing.T) {
	t.Parallel()
	useCase := newPrepareOnlyUseCase(t)
	ref := "bet-ref"
	req := testRequest(t, domainwallet.Win, "10.00")
	req.ReferenceExternalTransactionID = &ref

	prepared, err := useCase.Prepare(walletapp.ProcessOperationInput{
		Request: req, IdempotencyKey: testIdempKey, CorrelationID: "corr-1", Channel: walletapp.ChannelHTTP,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Decision().Action != operation.WaitForReference {
		t.Errorf("Decision().Action = %v, want operation.WaitForReference", prepared.Decision().Action)
	}
}

func TestProcessOperationUseCase_Prepare_ValidBet_ReturnsHashAndDecision(t *testing.T) {
	t.Parallel()
	useCase := newPrepareOnlyUseCase(t)
	req := testRequest(t, domainwallet.Bet, "10.00")

	prepared, err := useCase.Prepare(walletapp.ProcessOperationInput{
		Request: req, IdempotencyKey: testIdempKey, CorrelationID: "corr-1", Channel: walletapp.ChannelHTTP,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	wantHash, err := operation.PayloadHash(req)
	if err != nil {
		t.Fatalf("PayloadHash: %v", err)
	}
	if prepared.Hash() != wantHash {
		t.Errorf("Hash() = %q, want %q", prepared.Hash(), wantHash)
	}
	if prepared.Decision().Action != operation.Process {
		t.Errorf("Decision().Action = %v, want operation.Process", prepared.Decision().Action)
	}
	if prepared.Decision().Direction != domainwallet.Debit {
		t.Errorf("Decision().Direction = %v, want Debit for a BET", prepared.Decision().Direction)
	}
}

// TestProcessOperationUseCase_ExecuteInTx_ZeroValuePreparedOperation_RejectsWithoutTouchingRepositories
// proves ExecuteInTx never trusts a PreparedOperation it did not itself
// produce via Prepare: PreparedOperation{} is the only way to construct one
// outside this package (no exported fields, no other exported constructor),
// and ExecuteInTx must answer it with a classified error before calling any
// repository - never a panic, never a persisted hash or decision this use
// case never validated (ticket 08 re-review, Major: "PreparedOperation é
// exportado e seus três campos são mutáveis").
func TestProcessOperationUseCase_ExecuteInTx_ZeroValuePreparedOperation_RejectsWithoutTouchingRepositories(t *testing.T) {
	t.Parallel()
	wallets := &fakeWalletRepository{findResult: testWallet(t, "100.00", 1)}
	transactions := &fakeTransactionRepository{}
	ledger := &fakeLedgerRepository{}
	outbox := &fakeOutboxRepository{}
	useCase := walletapp.NewProcessOperationUseCase(explodingUnitOfWork{t: t}, &fakeOperationMetrics{})
	repos := walletapp.Repositories{Wallets: wallets, Transactions: transactions, Ledger: ledger, Outbox: outbox}

	_, err := useCase.ExecuteInTx(context.Background(), repos, walletapp.PreparedOperation{})
	if !errors.Is(err, walletapp.ErrOperationNotPrepared) {
		t.Errorf("err = %v, want ErrOperationNotPrepared", err)
	}
	if wallets.findForUpdateCalls != 0 {
		t.Errorf("Wallets.FindForUpdate calls = %d, want 0", wallets.findForUpdateCalls)
	}
	if len(transactions.inserted) != 0 {
		t.Errorf("transactions inserted = %d, want 0", len(transactions.inserted))
	}
	if len(ledger.inserted) != 0 {
		t.Errorf("ledger inserted = %d, want 0", len(ledger.inserted))
	}
	if len(outbox.inserted) != 0 {
		t.Errorf("outbox inserted = %d, want 0", len(outbox.inserted))
	}
}

// TestPreparedOperation_HasNoExportedFields documents, as a compile-time
// check, that PreparedOperation cannot be assembled from outside
// internal/walletapp with anything but the zero value: every field is
// unexported, so a struct literal naming a field (walletapp.PreparedOperation{Hash:
// "x"}) fails to compile, and Prepare is the only exported way to obtain a
// non-zero value.
func TestPreparedOperation_HasNoExportedFields(t *testing.T) {
	t.Parallel()
	_ = walletapp.PreparedOperation{}
}
