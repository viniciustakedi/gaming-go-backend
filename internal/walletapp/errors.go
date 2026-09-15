package walletapp

import (
	"errors"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
)

// ClassifyMoneyError maps a money.Error - whichever one Money.UnmarshalJSON
// or a domain constructor returned - onto the stable operation.Code
// catalog every HTTP error body reports. It is exported so
// internal/httpapi can apply the exact same mapping to a decode error that
// never reaches a use case at all (spec: "todo corpo de erro diz a qual
// [grupo] pertence").
func ClassifyMoneyError(err error) *operation.Error {
	if errors.Is(err, money.ErrUnsupportedCurrency) {
		return operation.ErrUnsupportedCurrency
	}
	return operation.ErrInvalidMoney
}
