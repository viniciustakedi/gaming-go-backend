package walletpg

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/pg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

// unitOfWork implements walletapp.UnitOfWork against a real *pg.UnitOfWork:
// the one place that builds the pgx-backed repositories, one fresh set per
// call, all bound to the transaction pg.UnitOfWork.Execute opens - so
// internal/walletapp never has to know pgx exists.
type unitOfWork struct {
	uow *pg.UnitOfWork
}

// NewUnitOfWork builds the walletapp.UnitOfWork port from *pg.UnitOfWork.
// Exported so a test that needs the real transaction boundary but a
// substituted repository can build one directly, without going through Fx.
func NewUnitOfWork(uow *pg.UnitOfWork) walletapp.UnitOfWork {
	return &unitOfWork{uow: uow}
}

func (u *unitOfWork) WithinTx(ctx context.Context, fn func(ctx context.Context, repos walletapp.Repositories) error) error {
	return u.uow.Execute(ctx, func(ctx context.Context, q pg.Querier) error {
		return fn(ctx, NewRepositories(q))
	})
}

// NewRepositories builds the full walletapp.Repositories bundle bound to q,
// the exact set unitOfWork.WithinTx binds to its own transaction above.
// Exported so a caller that already holds its own transaction can bind
// ExecuteInTx to it directly, without opening a second transaction.
func NewRepositories(q pg.Querier) walletapp.Repositories {
	return walletapp.Repositories{
		Wallets:      newWalletRepository(q),
		Transactions: newWagerTransactionRepository(q),
		Ledger:       newLedgerRepository(q),
		Outbox:       newOutboxRepository(q),
	}
}

// NewWalletReader builds the WalletRepository bound directly to the pool,
// for GetWalletUseCase's read, which needs no transaction boundary.
func NewWalletReader(pool *pgxpool.Pool) walletapp.WalletRepository {
	return newWalletRepository(pool)
}

// NewTransactionReader builds the WagerTransactionRepository bound directly
// to the pool, for GetTransactionUseCase's reads, which need no transaction
// boundary.
func NewTransactionReader(pool *pgxpool.Pool) walletapp.WagerTransactionRepository {
	return newWagerTransactionRepository(pool)
}

// Module provides the pgx-backed unit of work, the read-only repositories
// and, from them, the application use cases internal/httpapi calls. It
// depends on internal/pg's postgres module for *pg.UnitOfWork and
// *pgxpool.Pool, and comes before internal/httpapi in the composition root.
var Module = fx.Module("wallet",
	fx.Provide(
		NewUnitOfWork,
		NewWalletReader,
		NewTransactionReader,
		newLedgerAuditRepository,
		walletapp.NewOpenWalletUseCase,
		walletapp.NewGetWalletUseCase,
		walletapp.NewLedgerAuditUseCase,
		walletapp.NewProcessOperationUseCase,
		walletapp.NewGetTransactionUseCase,
	),
)
