package walletapp

import (
	"context"
	"errors"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

// Caller identifies who is asking to read a transaction: an authenticated
// provider, scoped to ProviderID, or the internal wallet-admin service,
// which sees every transaction, including the INTERNAL OPENING row.
type Caller struct {
	ProviderID string
	IsAdmin    bool
}

// GetTransactionUseCase resolves the two wagering-transaction read routes.
// Both are pure reads: no UnitOfWork, no transaction boundary.
type GetTransactionUseCase struct {
	transactions WagerTransactionRepository
}

// NewGetTransactionUseCase wires the use case to its repository.
func NewGetTransactionUseCase(transactions WagerTransactionRepository) *GetTransactionUseCase {
	return &GetTransactionUseCase{transactions: transactions}
}

// ByID resolves GET /wagering/transactions/:transactionId. A provider only
// ever sees its own EXTERNAL rows; anything else - another provider's row,
// or the INTERNAL OPENING row - answers exactly like a row that does not
// exist, so a provider probing ids can never learn which ones are real.
// wallet-admin sees every row unconditionally.
func (uc *GetTransactionUseCase) ByID(ctx context.Context, id string, caller Caller) (*TransactionDetail, error) {
	detail, err := uc.transactions.FindDetailByID(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, operation.ErrTransactionNotFound
		}
		return nil, err
	}
	if caller.IsAdmin {
		return detail, nil
	}
	if detail.Origin != domainwallet.External || detail.ProviderID != caller.ProviderID {
		return nil, operation.ErrTransactionNotFound
	}
	return detail, nil
}

// ByProviderExternalID resolves GET
// /providers/:providerId/wagering/transactions/:externalTransactionId. The
// route's own providerId governs the lookup: a provider caller must match it
// exactly (ErrForbiddenProvider otherwise), while wallet-admin may pass any
// providerId. The mismatch check runs before the query, so a wrong providerId
// never even reaches the repository.
func (uc *GetTransactionUseCase) ByProviderExternalID(ctx context.Context, routeProviderID, externalTransactionID string, caller Caller) (*TransactionDetail, error) {
	if !caller.IsAdmin && caller.ProviderID != routeProviderID {
		return nil, ErrForbiddenProvider
	}
	detail, err := uc.transactions.FindDetailByProviderExternalID(ctx, routeProviderID, externalTransactionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, operation.ErrTransactionNotFound
		}
		return nil, err
	}
	return detail, nil
}
