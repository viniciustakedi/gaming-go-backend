//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletpg"
)

// explodingUnitOfWork fails the test the instant WithinTx is called. Wiring
// ProcessOperationUseCase to it and driving ExecuteInTx directly proves
// ExecuteInTx never opens a transaction of its own - the shape the SQS
// consumer needs, binding ExecuteInTx to its own inbox transaction instead.
type explodingUnitOfWork struct{ t *testing.T }

func (u explodingUnitOfWork) WithinTx(context.Context, func(context.Context, walletapp.Repositories) error) error {
	u.t.Helper()
	u.t.Fatal("ExecuteInTx must never call UnitOfWork.WithinTx")
	return nil
}

// noopOperationMetrics satisfies walletapp.OperationMetrics without a real
// Prometheus registry - ExecuteInTx's own metrics calls are not what this
// test is about.
type noopOperationMetrics struct{}

func (noopOperationMetrics) ObserveOperation(string, string, string, time.Duration) {}
func (noopOperationMetrics) ObserveDuplicate(string)                                {}
func (noopOperationMetrics) ObserveConcurrencyConflict()                            {}

// TestProcessOperationExecuteInTx_ExternalTransactionRollback_PersistsNothing
// binds ExecuteInTx to a transaction this test itself opens and rolls back
// - never the UnitOfWork the HTTP handler uses - and checks nothing it
// wrote (wallet balance/version, the wager transaction, the ledger entry,
// the outbox records) survives that rollback. This is the composition the SQS
// consumer needs: ExecuteInTx bound to the same transaction the inbox row
// commits in, so a failure before that transaction's own commit can never
// leave the wager movement committed without the inbox record.
func TestProcessOperationExecuteInTx_ExternalTransactionRollback_PersistsNothing(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	ledgerBaseline := countLedgerEntries(t, ctx, h, wallet.ID)
	outboxBaseline := len(queryOutboxEvents(t, ctx, h, wallet.ID))

	useCase := walletapp.NewProcessOperationUseCase(explodingUnitOfWork{t: t}, noopOperationMetrics{})

	tx, err := h.pool.Begin(ctx)
	requireNoError(t, err, "begin external transaction")
	defer func() { _ = tx.Rollback(ctx) }() // no-op once the test's own Rollback below has run

	amount, err := money.Parse("30.00", money.BRL)
	requireNoError(t, err, "parse bet amount")

	prepared, err := useCase.Prepare(walletapp.ProcessOperationInput{
		Request: operation.Request{
			ProviderID: "provider-a", ExternalTransactionID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
			RoundID: uniqueID("round"), GameID: "game-1", Kind: domainwallet.Bet, Money: amount,
		},
		IdempotencyKey: "idem-" + uniqueID("k"), CorrelationID: "corr-1", Channel: walletapp.ChannelHTTP,
	})
	requireNoError(t, err, "Prepare")

	result, err := useCase.ExecuteInTx(ctx, walletpg.NewRepositories(tx), prepared)
	requireNoError(t, err, "ExecuteInTx")
	if result.Status != domainwallet.Processed || result.IdempotentReplay {
		t.Fatalf("result = %+v, want PROCESSED and not a replay, before the external transaction ever commits", result)
	}

	requireNoError(t, tx.Rollback(ctx), "rollback external transaction")

	balance, version, found := queryWalletRow(t, ctx, h, wallet.ID)
	if !found || balance != 10000 || version != 1 {
		t.Errorf("stored wallet = (balance %d, version %d, found %v), want unchanged (10000, 1, true) after rollback", balance, version, found)
	}
	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != ledgerBaseline {
		t.Errorf("ledger entries = %d, want unchanged %d after rollback", got, ledgerBaseline)
	}
	if got := len(queryOutboxEvents(t, ctx, h, wallet.ID)); got != outboxBaseline {
		t.Errorf("outbox events for wallet = %d, want unchanged %d after rollback", got, outboxBaseline)
	}
	if _, _, _, found := queryWagerTransaction(t, ctx, h, result.TransactionID); found {
		t.Errorf("wager transaction %s exists after the external transaction rolled back, want none", result.TransactionID)
	}
}
