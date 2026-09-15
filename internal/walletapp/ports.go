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

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

// ErrNotFound is returned by WalletRepository.FindByID/FindForUpdate and by
// WagerTransactionRepository's two lookups when no row matches. It is a
// repository-level sentinel, not an HTTP-facing failure code: the use case
// is what translates it to the right operation.Error, keeping this
// package's repositories ignorant of the HTTP error catalog.
var ErrNotFound = errors.New("walletapp: not found")

// ErrAlreadyExists is returned by WalletRepository.Insert when the
// (playerId, currency) pair already has a wallet - detected by the
// database's own unique constraint, which is also what makes concurrent
// opens for the same pair safe: only one INSERT can win.
var ErrAlreadyExists = errors.New("walletapp: wallet already exists")

// ErrConcurrencyConflict is returned by WalletRepository.UpdateBalance when
// the row's version no longer matches the one the caller read under its own
// FOR UPDATE lock. This is defense in depth (spec: "as garantias no banco
// independem do lock") - it is never expected to fire in practice, since the
// lock is held for the whole processing attempt.
var ErrConcurrencyConflict = errors.New("walletapp: wallet version changed concurrently")

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
	// FindForUpdate reads the wallet row locked with SELECT ... FOR UPDATE,
	// so the caller has exclusive use of it for the rest of its transaction
	// (spec, decision 3: "SELECT ... FOR UPDATE na linha da carteira").
	FindForUpdate(ctx context.Context, id string) (*domainwallet.Wallet, error)
	// UpdateBalance persists w's current balance and version, conditioned on
	// previousVersion - the version the caller read at lock time - and
	// returns ErrConcurrencyConflict if the row has since moved on.
	UpdateBalance(ctx context.Context, w *domainwallet.Wallet, previousVersion int64) error
}

// ExistingTransaction is the persisted state of an external wager
// transaction, read back either to classify an idempotency attempt
// (IdempotencyKey, PayloadHash, ExternalTransactionID feed
// operation.ClassifyAttempt) or, on replay, to answer with the exact result
// the original processing computed (Status, FailureCode, ResultingBalance).
type ExistingTransaction struct {
	TransactionID         string
	IdempotencyKey        string
	PayloadHash           string
	ExternalTransactionID string
	Status                domainwallet.TransactionStatus
	FailureCode           string
	ResultingBalance      *int64
	Currency              money.Currency
}

// WagerTransactionRepository persists and looks up a wager transaction.
// resultingBalance is nil for statuses that never carry one (spec:
// wager_transactions_resulting_balance_terminal_check).
type WagerTransactionRepository interface {
	// Insert writes an already-terminal row unconditionally - used only for
	// the OPENING credit, which never competes with anything on the two
	// idempotency unique constraints (both columns are NULL for an
	// INTERNAL row).
	Insert(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) error
	// InsertNew writes one external wager_transactions row, silently doing
	// nothing if a concurrent writer already committed the same
	// (providerId, idempotencyKey) or (providerId, externalTransactionId)
	// pair - the backstop for a duplicate that used a different walletId in
	// its body and so never contended for the same FOR UPDATE lock (spec,
	// decision 3, step 5: "INSERT ... ON CONFLICT DO NOTHING"). inserted
	// reports which of the two happened, so the caller knows whether it
	// must roll back and reclassify in a fresh transaction.
	InsertNew(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) (inserted bool, err error)
	// FindByIdempotencyKey and FindByExternalTransactionID each return
	// ErrNotFound when no row matches; both are scoped to providerID, since
	// idempotency keys and external transaction ids are only unique per
	// provider (spec: "o escopo de chaves é por provedor").
	FindByIdempotencyKey(ctx context.Context, providerID, idempotencyKey string) (*ExistingTransaction, error)
	FindByExternalTransactionID(ctx context.Context, providerID, externalTransactionID string) (*ExistingTransaction, error)
	// FindReference resolves the transaction a REFUND, ROLLBACK or a
	// referenced WIN names, scoped to the same provider the operation
	// itself came from (spec: "a referência é resolvida por (providerId,
	// referenceExternalTransactionId)"). It returns ErrNotFound when no
	// such transaction has arrived yet; the caller - not this port -
	// decides what an unresolved reference means.
	FindReference(ctx context.Context, providerID, referenceExternalTransactionID string) (*domainwallet.WagerTransaction, error)
	// ExistsSuccessfulReversal reports whether referenceTransactionID
	// already has a PROCESSED REFUND or ROLLBACK against it (spec: "uma
	// BET aceita uma única reversão bem-sucedida"). Only meaningful once
	// the reference itself is PROCESSED.
	ExistsSuccessfulReversal(ctx context.Context, referenceTransactionID string) (bool, error)
}

// LedgerRepository appends one immutable ledger entry.
type LedgerRepository interface {
	Insert(ctx context.Context, entry *domainwallet.WalletLedgerEntry) error
}

// LedgerEntry is the externally auditable projection of one immutable ledger row.
type LedgerEntry struct {
	SequenceNumber int64
	TransactionID  string
	Direction      domainwallet.Direction
	Money          money.Money
	BalanceBefore  money.Money
	BalanceAfter   money.Money
	OccurredAt     time.Time
}

// LedgerPage is a stable, keyset-paginated segment of one wallet's ledger.
type LedgerPage struct {
	Entries []LedgerEntry
	Next    *int64
}

// Reconciliation is the result of rebuilding a wallet balance from its ledger.
type Reconciliation struct {
	WalletID          string
	StoredBalance     money.Money
	CalculatedBalance money.Money
	Difference        money.Money
	CheckedEntries    int64
}

// LedgerAuditRepository is the read side of the ledger. Reconcile must use one
// repeatable-read, read-only database snapshot for every value it returns.
type LedgerAuditRepository interface {
	List(ctx context.Context, walletID string, afterSequence int64, limit int) (LedgerPage, error)
	Reconcile(ctx context.Context, walletID string) (Reconciliation, error)
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

// OperationMetrics records the observability signals ProcessOperationUseCase
// emits for every attempt, regardless of channel (HTTP today; SQS once
// ticket 13 reuses this use case). It is a port, not a direct
// prometheus/client_golang dependency, so this package stays free of a
// concrete metrics library (spec: "Camadas"); internal/wageringmetrics
// implements it.
type OperationMetrics interface {
	// ObserveOperation records one attempt's outcome and how long it took,
	// labeled by channel, operation kind and resulting status (spec:
	// "operações por canal, tipo e estado" and "histograma de latência de
	// processamento").
	ObserveOperation(channel, kind, status string, duration time.Duration)
	// ObserveDuplicate records one idempotent replay, by channel (spec:
	// "duplicatas por canal").
	ObserveDuplicate(channel string)
	// ObserveConcurrencyConflict records one detected concurrency conflict:
	// the step-5 INSERT backstop finding nothing to insert, or the wallet's
	// version check failing on UPDATE (spec: "um conflito de concorrência
	// detectado ... incrementa uma métrica").
	ObserveConcurrencyConflict()
}
