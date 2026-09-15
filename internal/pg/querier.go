package pg

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the minimal pgx surface a repository needs to run a query. Both
// *pgxpool.Pool and pgx.Tx satisfy it, so a repository method can be called
// either directly against the pool - for a read that needs no transaction -
// or against a transaction the UnitOfWork opened, without the repository
// itself ever knowing which one it got or being able to begin, commit or
// roll one back (spec: "os repositórios nunca abrem transação por conta
// própria").
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// UnitOfWork is the one place in the application that opens a pgx
// transaction. A use case calls Execute and gets back a Querier already
// bound to that transaction to hand to every repository call it makes;
// Reader hands out the pool itself for read-only calls that need no
// transaction boundary at all (spec: "a unidade de trabalho abre uma
// transação pgx e entrega aos repositórios uma interface de consulta ligada
// a ela").
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
// documented pgx no-op, so calling Rollback unconditionally after a failed
// Commit is safe.
func (u *UnitOfWork) Execute(ctx context.Context, fn func(ctx context.Context, q Querier) error) error {
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return nil
}
