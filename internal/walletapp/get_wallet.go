package walletapp

import (
	"context"
	"errors"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

// GetWalletUseCase reads a wallet's current balance and version.
type GetWalletUseCase struct {
	wallets WalletRepository
}

// NewGetWalletUseCase wires the use case to its repository. A read needs no
// transaction, so this depends on WalletRepository directly, not UnitOfWork.
func NewGetWalletUseCase(wallets WalletRepository) *GetWalletUseCase {
	return &GetWalletUseCase{wallets: wallets}
}

// Get looks up a wallet by id.
func (uc *GetWalletUseCase) Get(ctx context.Context, id string) (*domainwallet.Wallet, error) {
	walletValue, err := uc.wallets.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, operation.ErrWalletNotFound
		}
		return nil, err
	}
	return walletValue, nil
}
