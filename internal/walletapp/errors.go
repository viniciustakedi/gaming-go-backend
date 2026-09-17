package walletapp

import (
	"errors"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
)

// ClassifyMoneyError maps a money.Error onto the stable operation.Code
// catalog every HTTP error body reports. It is exported so internal/httpapi
// can apply the exact same mapping to a decode error that never reaches a
// use case at all.
func ClassifyMoneyError(err error) *operation.Error {
	if errors.Is(err, money.ErrUnsupportedCurrency) {
		return operation.ErrUnsupportedCurrency
	}
	return operation.ErrInvalidMoney
}
