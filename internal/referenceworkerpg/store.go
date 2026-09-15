// Package referenceworkerpg is the PostgreSQL adapter for the pending-reference worker.
package referenceworkerpg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/referenceworker"
)

type store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) referenceworker.Store { return &store{pool: pool} }

func (s *store) Claim(ctx context.Context, batch int, lease time.Duration) ([]referenceworker.Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		-- next_attempt_at is both retry deadline and lease. SKIP LOCKED lets
		-- instances divide ready rows without waiting; a crash releases work
		-- when the PostgreSQL-clock lease expires.
		WITH ready AS (
			SELECT id FROM wager_transactions
			WHERE status = 'PENDING_REFERENCE' AND next_attempt_at <= now()
			ORDER BY next_attempt_at, created_at
			FOR UPDATE SKIP LOCKED LIMIT $2
		)
		UPDATE wager_transactions AS t SET next_attempt_at = now() + $1::interval
		FROM ready WHERE t.id = ready.id
		RETURNING t.id, t.wallet_id, t.attempts`, lease.String(), batch)
	if err != nil {
		return nil, fmt.Errorf("claim pending references: %w", err)
	}
	defer rows.Close()
	var claimed []referenceworker.Record
	for rows.Next() {
		var record referenceworker.Record
		if err := rows.Scan(&record.TransactionID, &record.WalletID, &record.Attempts); err != nil {
			return nil, fmt.Errorf("scan claim: %w", err)
		}
		claimed = append(claimed, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return claimed, nil
}

func (s *store) Pending(ctx context.Context) (int, error) {
	var pending int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE status = 'PENDING_REFERENCE'`).Scan(&pending); err != nil {
		return 0, fmt.Errorf("count pending references: %w", err)
	}
	return pending, nil
}

var Module = fx.Module("referenceworkerpg", fx.Provide(NewStore))
