package walletapp

import (
	"context"
	"errors"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
)

// LedgerAuditUseCase exposes the wallet-admin audit reads without coupling
// HTTP to the Postgres adapter that owns their transaction boundary.
type LedgerAuditUseCase struct {
	wallets WalletRepository
	ledger  LedgerAuditRepository
}

func NewLedgerAuditUseCase(wallets WalletRepository, ledger LedgerAuditRepository) *LedgerAuditUseCase {
	return &LedgerAuditUseCase{wallets: wallets, ledger: ledger}
}

func (uc *LedgerAuditUseCase) List(ctx context.Context, walletID string, afterSequence int64, limit int) (LedgerPage, error) {
	if _, err := uc.wallets.FindByID(ctx, walletID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return LedgerPage{}, operation.ErrWalletNotFound
		}
		return LedgerPage{}, err
	}
	return uc.ledger.List(ctx, walletID, afterSequence, limit)
}

func (uc *LedgerAuditUseCase) Reconcile(ctx context.Context, walletID string) (Reconciliation, error) {
	result, err := uc.ledger.Reconcile(ctx, walletID)
	if errors.Is(err, ErrNotFound) {
		return Reconciliation{}, operation.ErrWalletNotFound
	}
	return result, err
}
