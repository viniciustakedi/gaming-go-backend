package walletapp_test

import (
	"context"
	"testing"
	"time"

	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

// fakeUnitOfWork runs fn directly against whichever fake repositories the test
// constructed it with: these tests cover the use case's own decisions, not SQL
// or transaction boundaries. calls counts every WithinTx invocation, so a test
// can pin that a correctable input never opens a transaction at all.
type fakeUnitOfWork struct {
	wallets      walletapp.WalletRepository
	transactions walletapp.WagerTransactionRepository
	ledger       walletapp.LedgerRepository
	outbox       walletapp.OutboxRepository
	calls        int
}

func (f *fakeUnitOfWork) WithinTx(ctx context.Context, fn func(context.Context, walletapp.Repositories) error) error {
	f.calls++
	return fn(ctx, walletapp.Repositories{
		Wallets:      f.wallets,
		Transactions: f.transactions,
		Ledger:       f.ledger,
		Outbox:       f.outbox,
	})
}

// explodingUnitOfWork fails the test the instant WithinTx is called, so a test
// driving ExecuteInTx directly pins that it never opens a transaction of its
// own - it has to be able to join a caller's external transaction.
type explodingUnitOfWork struct{ t *testing.T }

func (f explodingUnitOfWork) WithinTx(context.Context, func(context.Context, walletapp.Repositories) error) error {
	f.t.Helper()
	f.t.Fatal("ExecuteInTx must never call UnitOfWork.WithinTx")
	return nil
}

type fakeWalletRepository struct {
	inserted           []*domainwallet.Wallet
	insertErr          error
	findResult         *domainwallet.Wallet
	findErr            error
	updated            []*domainwallet.Wallet
	updateErr          error
	forUpdateErr       error
	findForUpdateCalls int
}

func (f *fakeWalletRepository) Insert(ctx context.Context, w *domainwallet.Wallet) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = append(f.inserted, w)
	return nil
}

func (f *fakeWalletRepository) FindByID(ctx context.Context, id string) (*domainwallet.Wallet, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.findResult, nil
}

func (f *fakeWalletRepository) FindForUpdate(ctx context.Context, id string) (*domainwallet.Wallet, error) {
	f.findForUpdateCalls++
	if f.forUpdateErr != nil {
		return nil, f.forUpdateErr
	}
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.findResult, nil
}

func (f *fakeWalletRepository) UpdateBalance(ctx context.Context, w *domainwallet.Wallet, previousVersion int64) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = append(f.updated, w)
	return nil
}

type fakeTransactionRepository struct {
	inserted                []*domainwallet.WagerTransaction
	balances                []*int64
	err                     error
	insertNewConflict       bool
	insertNewAlwaysConflict bool
	insertNewErr            error
	byIdempotencyKey        *walletapp.ExistingTransaction
	byIdempotencyKeyErr     error
	byExternal              *walletapp.ExistingTransaction
	byExternalErr           error
	reference               *domainwallet.WagerTransaction
	referenceErr            error
	pending                 *walletapp.PendingReferenceTransaction
	pendingErr              error
	insertPendingExpiresAt  time.Time
	alreadyReversed         bool
	alreadyReversedErr      error
	detailByID              *walletapp.TransactionDetail
	detailByIDErr           error
	detailByExternal        *walletapp.TransactionDetail
	detailByExternalErr     error
}

func (f *fakeTransactionRepository) Insert(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) error {
	if f.err != nil {
		return f.err
	}
	f.inserted = append(f.inserted, t)
	f.balances = append(f.balances, resultingBalance)
	return nil
}

func (f *fakeTransactionRepository) InsertNew(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) (bool, error) {
	if f.insertNewErr != nil {
		return false, f.insertNewErr
	}
	if f.insertNewAlwaysConflict {
		// Simulates a collision that never resolves: unlike
		// insertNewConflict below, no record ever becomes visible, so a
		// caller that retries in a fresh transaction collides again -
		// proving a second collision in a row is not retried a third time.
		return false, nil
	}
	if f.insertNewConflict {
		// Simulates a concurrent writer, with a different walletId in its
		// body, having already committed this exact (providerId,
		// idempotencyKey) under a different lock - exactly what the ON
		// CONFLICT DO NOTHING backstop exists for. The record becomes
		// visible to the caller's reclassification-in-a-fresh-transaction,
		// the same way a real commit would.
		currency, _ := t.Money().Currency()
		f.byIdempotencyKey = &walletapp.ExistingTransaction{
			TransactionID: t.ID(), IdempotencyKey: t.IdempotencyKey(), PayloadHash: t.PayloadHash(),
			ExternalTransactionID: t.ExternalTransactionID(), Status: t.Status(), FailureCode: t.FailureCode(),
			ResultingBalance: resultingBalance, Currency: currency,
		}
		return false, nil
	}
	f.inserted = append(f.inserted, t)
	f.balances = append(f.balances, resultingBalance)
	return true, nil
}

func (f *fakeTransactionRepository) InsertPending(ctx context.Context, t *domainwallet.WagerTransaction, nextAttemptAt time.Time, ttl time.Duration) (time.Time, bool, error) {
	expiresAt := f.insertPendingExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(ttl)
	}
	inserted, err := f.InsertNew(ctx, t, nil)
	return expiresAt, inserted, err
}

func (f *fakeTransactionRepository) FindByIdempotencyKey(ctx context.Context, providerID, idempotencyKey string) (*walletapp.ExistingTransaction, error) {
	if f.byIdempotencyKeyErr != nil {
		return nil, f.byIdempotencyKeyErr
	}
	if f.byIdempotencyKey == nil {
		return nil, walletapp.ErrNotFound
	}
	return f.byIdempotencyKey, nil
}

func (f *fakeTransactionRepository) FindByExternalTransactionID(ctx context.Context, providerID, externalTransactionID string) (*walletapp.ExistingTransaction, error) {
	if f.byExternalErr != nil {
		return nil, f.byExternalErr
	}
	if f.byExternal == nil {
		return nil, walletapp.ErrNotFound
	}
	return f.byExternal, nil
}

func (f *fakeTransactionRepository) FindReference(ctx context.Context, providerID, referenceExternalTransactionID string) (*domainwallet.WagerTransaction, error) {
	if f.referenceErr != nil {
		return nil, f.referenceErr
	}
	if f.reference == nil {
		return nil, walletapp.ErrNotFound
	}
	return f.reference, nil
}

func (f *fakeTransactionRepository) ExistsSuccessfulReversal(ctx context.Context, referenceTransactionID string) (bool, error) {
	if f.alreadyReversedErr != nil {
		return false, f.alreadyReversedErr
	}
	return f.alreadyReversed, nil
}

func (f *fakeTransactionRepository) FindPendingForUpdate(ctx context.Context, transactionID string) (*walletapp.PendingReferenceTransaction, error) {
	if f.pendingErr != nil {
		return nil, f.pendingErr
	}
	if f.pending != nil {
		return f.pending, nil
	}
	return nil, walletapp.ErrNotFound
}
func (f *fakeTransactionRepository) ReschedulePending(ctx context.Context, transactionID string, attempts int, retryDelay time.Duration) error {
	return nil
}
func (f *fakeTransactionRepository) CompletePending(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) error {
	return nil
}

func (f *fakeTransactionRepository) FindDetailByID(ctx context.Context, id string) (*walletapp.TransactionDetail, error) {
	if f.detailByIDErr != nil {
		return nil, f.detailByIDErr
	}
	if f.detailByID == nil {
		return nil, walletapp.ErrNotFound
	}
	return f.detailByID, nil
}

func (f *fakeTransactionRepository) FindDetailByProviderExternalID(ctx context.Context, providerID, externalTransactionID string) (*walletapp.TransactionDetail, error) {
	if f.detailByExternalErr != nil {
		return nil, f.detailByExternalErr
	}
	if f.detailByExternal == nil {
		return nil, walletapp.ErrNotFound
	}
	return f.detailByExternal, nil
}

type fakeLedgerRepository struct {
	inserted []*domainwallet.WalletLedgerEntry
	err      error
}

func (f *fakeLedgerRepository) Insert(ctx context.Context, entry *domainwallet.WalletLedgerEntry) error {
	if f.err != nil {
		return f.err
	}
	f.inserted = append(f.inserted, entry)
	return nil
}

type fakeOutboxRecord struct {
	eventType     string
	aggregateType string
}

type fakeOutboxRepository struct {
	inserted []fakeOutboxRecord
	err      error
}

func (f *fakeOutboxRepository) Insert(ctx context.Context, record walletapp.OutboxRecord) error {
	if f.err != nil {
		return f.err
	}
	f.inserted = append(f.inserted, fakeOutboxRecord{eventType: record.EventType, aggregateType: record.AggregateType})
	return nil
}

// fakeOperationMetrics records every observation ProcessOperationUseCase
// makes, so a test can assert on channel/kind/status labels and counts
// without a real Prometheus registry.
type fakeOperationMetrics struct {
	operations []observedOperation
	duplicates []string
	conflicts  int
}

type observedOperation struct {
	channel, kind, status string
	duration              time.Duration
}

func (f *fakeOperationMetrics) ObserveOperation(channel, kind, status string, duration time.Duration) {
	f.operations = append(f.operations, observedOperation{channel: channel, kind: kind, status: status, duration: duration})
}

func (f *fakeOperationMetrics) ObserveDuplicate(channel string) {
	f.duplicates = append(f.duplicates, channel)
}

func (f *fakeOperationMetrics) ObserveConcurrencyConflict() {
	f.conflicts++
}
