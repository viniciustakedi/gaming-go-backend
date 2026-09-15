package walletapp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

func TestGetWalletUseCase_Found(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	want, err := domainwallet.Rehydrate(domainwallet.RehydratedWallet{
		ID: "wallet-1", PlayerID: "player-1", Currency: money.BRL, Balance: mustMoney(t, "50.00", money.BRL),
		Version: 3, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Rehydrate() error = %v", err)
	}

	uc := walletapp.NewGetWalletUseCase(&fakeWalletRepository{findResult: want})

	got, err := uc.Get(context.Background(), "wallet-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID() != "wallet-1" || got.Version() != 3 {
		t.Errorf("Get() = %+v, want id wallet-1 version 3", got)
	}
}

func TestGetWalletUseCase_NotFound_MapsToOperationError(t *testing.T) {
	t.Parallel()

	uc := walletapp.NewGetWalletUseCase(&fakeWalletRepository{findErr: walletapp.ErrNotFound})

	_, err := uc.Get(context.Background(), "missing")
	if !errors.Is(err, operation.ErrWalletNotFound) {
		t.Errorf("Get() error = %v, want errors.Is(_, operation.ErrWalletNotFound)", err)
	}
}

func TestGetWalletUseCase_UnexpectedReadFailure_PropagatesUnclassified(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset")
	uc := walletapp.NewGetWalletUseCase(&fakeWalletRepository{findErr: boom})

	_, err := uc.Get(context.Background(), "wallet-1")
	if !errors.Is(err, boom) {
		t.Errorf("Get() error = %v, want errors.Is(_, boom)", err)
	}
}
