package walletpg

import (
	"context"
	"fmt"

	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/pg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

type ledgerRepository struct{ q pg.Querier }

// newLedgerRepository builds the pgx-backed LedgerRepository bound to q.
func newLedgerRepository(q pg.Querier) walletapp.LedgerRepository {
	return &ledgerRepository{q: q}
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
