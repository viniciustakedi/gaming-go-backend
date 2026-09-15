package walletapp_test

import (
	"context"

	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

// fakeUnitOfWork runs fn directly, handing it whichever fake repositories
// the test constructed it with: the point of these tests is the use case's
// own decisions (what it builds, what it calls, how it classifies errors),
// not SQL or transaction boundaries - those are covered at seam 3a against
// a real Postgres.
type fakeUnitOfWork struct {
	wallets      walletapp.WalletRepository
	transactions walletapp.WagerTransactionRepository
	ledger       walletapp.LedgerRepository
	outbox       walletapp.OutboxRepository
}

func (f fakeUnitOfWork) WithinTx(ctx context.Context, fn func(context.Context, walletapp.Repositories) error) error {
	return fn(ctx, walletapp.Repositories{
		Wallets:      f.wallets,
		Transactions: f.transactions,
		Ledger:       f.ledger,
		Outbox:       f.outbox,
	})
}

type fakeWalletRepository struct {
	inserted   []*domainwallet.Wallet
	insertErr  error
	findResult *domainwallet.Wallet
	findErr    error
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

type fakeTransactionRepository struct {
	inserted []*domainwallet.WagerTransaction
	balances []*int64
	err      error
}

func (f *fakeTransactionRepository) Insert(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) error {
	if f.err != nil {
		return f.err
	}
	f.inserted = append(f.inserted, t)
	f.balances = append(f.balances, resultingBalance)
	return nil
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
