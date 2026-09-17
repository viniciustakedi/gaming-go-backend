package pg

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/faultinject"
)

// Querier is the minimal pgx surface a repository needs to run a query. Both
// *pgxpool.Pool and pgx.Tx satisfy it, so a repository method can run
// against the pool - for a read that needs no transaction - or against a
// transaction the UnitOfWork opened, without ever knowing which one it got
// or being able to begin, commit or roll one back.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// UnitOfWork is the one place in the application that opens a pgx
// transaction. A use case calls Execute and gets back a Querier already
// bound to that transaction to hand to every repository call it makes;
// Reader hands out the pool itself for read-only calls that need no
// transaction boundary at all.
type UnitOfWork struct {
	pool *pgxpool.Pool
}

// NewUnitOfWork builds a UnitOfWork bound to the application's pool.
func NewUnitOfWork(pool *pgxpool.Pool) *UnitOfWork {
	return &UnitOfWork{pool: pool}
}

// Reader returns a Querier bound directly to the pool, for repository calls
// that only read and need no transaction.
func (u *UnitOfWork) Reader() Querier {
	return u.pool
}

// Execute runs fn inside exactly one transaction: fn returning nil commits,
// anything else - fn's own error, or a failed commit - rolls back and
// returns that error. Rolling back an already-committed transaction is a
// documented pgx no-op, so the unconditional Rollback after a failed Commit
// is safe.
//
// The "before-commit" fault point fires here, right before Commit, for
// every caller that goes through this single transaction boundary. A no-op
// outside a `faultinject` build.
func (u *UnitOfWork) Execute(ctx context.Context, fn func(ctx context.Context, q Querier) error) error {
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	faultinject.Trigger("before-commit")
	if err := tx.Commit(ctx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return nil
}
