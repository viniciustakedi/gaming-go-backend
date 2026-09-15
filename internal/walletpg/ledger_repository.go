package walletpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/pg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

type ledgerRepository struct{ q pg.Querier }

// newLedgerRepository builds the pgx-backed LedgerRepository bound to q.
func newLedgerRepository(q pg.Querier) walletapp.LedgerRepository {
	return &ledgerRepository{q: q}
}

type ledgerAuditRepository struct{ pool *pgxpool.Pool }

func newLedgerAuditRepository(pool *pgxpool.Pool) walletapp.LedgerAuditRepository {
	return &ledgerAuditRepository{pool: pool}
}

func (r *ledgerAuditRepository) List(ctx context.Context, walletID string, afterSequence int64, limit int) (walletapp.LedgerPage, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT sequence_number, transaction_id, direction, amount, currency, balance_before, balance_after, occurred_at
		FROM wallet_ledger_entries
		WHERE wallet_id = $1 AND sequence_number > $2
		ORDER BY sequence_number
		LIMIT $3`, walletID, afterSequence, limit+1)
	if err != nil {
		return walletapp.LedgerPage{}, fmt.Errorf("walletpg: list ledger: %w", err)
	}
	defer rows.Close()

	entries, err := scanLedgerEntries(rows)
	if err != nil {
		return walletapp.LedgerPage{}, err
	}
	page := walletapp.LedgerPage{Entries: entries}
	if len(entries) > limit {
		next := entries[limit-1].SequenceNumber
		page.Entries = entries[:limit]
		page.Next = &next
	}
	return page, nil
}

func (r *ledgerAuditRepository) Reconcile(ctx context.Context, walletID string) (result walletapp.Reconciliation, err error) {
	// A single repeatable-read, read-only transaction makes the wallet row and
	// ledger aggregate one snapshot while making a balance mutation impossible.
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, fmt.Errorf("walletpg: begin reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var stored, calculated int64
	var currency string
	if err := tx.QueryRow(ctx, `SELECT balance, currency FROM wallets WHERE id = $1`, walletID).Scan(&stored, &currency); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, walletapp.ErrNotFound
		}
		return result, fmt.Errorf("walletpg: read reconciliation wallet: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN direction = 'CREDIT' THEN amount ELSE -amount END), 0), count(*)
		FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&calculated, &result.CheckedEntries); err != nil {
		return result, fmt.Errorf("walletpg: sum reconciliation ledger: %w", err)
	}
	result.WalletID = walletID
	result.StoredBalance, err = money.New(stored, money.Currency(currency))
	if err != nil {
		return result, fmt.Errorf("walletpg: decode stored balance: %w", err)
	}
	result.CalculatedBalance, err = money.New(calculated, money.Currency(currency))
	if err != nil {
		return result, fmt.Errorf("walletpg: decode calculated balance: %w", err)
	}
	result.Difference, err = result.StoredBalance.Subtract(result.CalculatedBalance)
	if err != nil {
		return result, fmt.Errorf("walletpg: calculate reconciliation difference: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("walletpg: commit reconciliation: %w", err)
	}
	return result, nil
}

func scanLedgerEntries(rows pgx.Rows) ([]walletapp.LedgerEntry, error) {
	var entries []walletapp.LedgerEntry
	for rows.Next() {
		var entry walletapp.LedgerEntry
		var amount, before, after int64
		var currency string
		if err := rows.Scan(&entry.SequenceNumber, &entry.TransactionID, &entry.Direction, &amount, &currency, &before, &after, &entry.OccurredAt); err != nil {
			return nil, fmt.Errorf("walletpg: scan ledger entry: %w", err)
		}
		var err error
		entry.Money, err = money.New(amount, money.Currency(currency))
		if err != nil {
			return nil, fmt.Errorf("walletpg: decode ledger money: %w", err)
		}
		entry.BalanceBefore, err = money.New(before, money.Currency(currency))
		if err != nil {
			return nil, fmt.Errorf("walletpg: decode ledger balance before: %w", err)
		}
		entry.BalanceAfter, err = money.New(after, money.Currency(currency))
		if err != nil {
			return nil, fmt.Errorf("walletpg: decode ledger balance after: %w", err)
		}
		entry.OccurredAt = entry.OccurredAt.UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("walletpg: iterate ledger entries: %w", err)
	}
	return entries, nil
}

func (r *ledgerRepository) Insert(ctx context.Context, entry *domainwallet.WalletLedgerEntry) error {
	amount, err := entry.Money().MinorUnits()
	if err != nil {
		return fmt.Errorf("walletpg: ledger amount: %w", err)
	}
	currency, err := entry.Money().Currency()
	if err != nil {
		return fmt.Errorf("walletpg: ledger currency: %w", err)
	}
	before, err := entry.BalanceBefore().MinorUnits()
	if err != nil {
		return fmt.Errorf("walletpg: ledger balance before: %w", err)
	}
	after, err := entry.BalanceAfter().MinorUnits()
	if err != nil {
		return fmt.Errorf("walletpg: ledger balance after: %w", err)
	}

	_, err = r.q.Exec(ctx, `
		INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount, currency, balance_before, balance_after, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		entry.ID(), entry.WalletID(), entry.TransactionID(), string(entry.Direction()), amount, string(currency), before, after, entry.OccurredAt())
	if err != nil {
		return fmt.Errorf("walletpg: insert ledger entry: %w", err)
	}
	return nil
}
