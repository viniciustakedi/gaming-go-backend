// Package walletapp is the application layer for wallet use cases: it
// depends only on the pure domain (internal/domain/wallet,
// internal/domain/money, internal/domain/operation), never on a concrete
// Postgres adapter, pgx, Fx or HTTP - those live one layer out, in
// internal/walletpg and internal/httpapi (spec: "Camadas: Domínio puro ->
// aplicação -> adaptadores -> composição"). The unit of work and every
// repository are ports this package declares and internal/walletpg
// implements; nothing here imports internal/pg.
package walletapp

import (
	"context"
	"errors"
	"time"

	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

// ErrNotFound is returned by WalletRepository.FindByID when no row matches.
// It is a repository-level sentinel, not an HTTP-facing failure code: the
// use case is what translates it to operation.ErrWalletNotFound, keeping
// this package's repositories ignorant of the HTTP error catalog.
var ErrNotFound = errors.New("walletapp: wallet not found")

// ErrAlreadyExists is returned by WalletRepository.Insert when the
// (playerId, currency) pair already has a wallet - detected by the
// database's own unique constraint, which is also what makes concurrent
// opens for the same pair safe: only one INSERT can win.
var ErrAlreadyExists = errors.New("walletapp: wallet already exists")

// Repositories bundles every repository port a unit of work hands to the
// function passed to UnitOfWork.WithinTx, all bound to that same
// transaction so a use case never has to thread a transaction handle
// through its own arguments.
type Repositories struct {
	Wallets      WalletRepository
	Transactions WagerTransactionRepository
	Ledger       LedgerRepository
	Outbox       OutboxRepository
}

// UnitOfWork is the one port every write use case in this package depends
// on. WithinTx runs fn inside exactly one transaction, handing it
// repositories bound to that transaction; a fake in a unit test can run fn
// directly, with repositories of its own choosing, and never needs a real
// Postgres.
type UnitOfWork interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, repos Repositories) error) error
}

// WalletRepository persists and loads the wallet aggregate.
type WalletRepository interface {
	Insert(ctx context.Context, w *domainwallet.Wallet) error
	FindByID(ctx context.Context, id string) (*domainwallet.Wallet, error)
}

// WagerTransactionRepository persists a wager transaction. resultingBalance
// is nil for statuses that never carry one (spec:
// wager_transactions_resulting_balance_terminal_check); this ticket only
// ever inserts an already-PROCESSED OPENING row, so it is always supplied
// here.
type WagerTransactionRepository interface {
	Insert(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) error
}

// LedgerRepository appends one immutable ledger entry.
type LedgerRepository interface {
	Insert(ctx context.Context, entry *domainwallet.WalletLedgerEntry) error
}

// OutboxRecord is the row-level shape OutboxRepository persists. Payload is
// marshaled to JSON by the repository, not by the use case, so this package
// never has to know the storage encoding.
type OutboxRecord struct {
	EventID       string
	EventType     string
	AggregateType string
	AggregateID   string
	EventVersion  int
	OccurredAt    time.Time
	Payload       any
}

// OutboxRepository appends one transactional-outbox record, in the same
// commit as the domain state it announces.
type OutboxRepository interface {
	Insert(ctx context.Context, record OutboxRecord) error
}
