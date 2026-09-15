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

func externalDetail(t *testing.T, providerID string) *walletapp.TransactionDetail {
	t.Helper()
	now := time.Now().UTC()
	return &walletapp.TransactionDetail{
		TransactionID: "tx-1", ExternalTransactionID: "ext-1", ProviderID: providerID,
		PlayerID: "player-1", WalletID: "wallet-1", RoundID: "round-1", GameID: "game-1",
		Kind: domainwallet.Bet, Origin: domainwallet.External, Money: mustMoney(t, "10.00", money.BRL),
		Status: domainwallet.Processed, CreatedAt: now, UpdatedAt: now,
	}
}

func openingDetail(t *testing.T) *walletapp.TransactionDetail {
	t.Helper()
	now := time.Now().UTC()
	return &walletapp.TransactionDetail{
		TransactionID: "tx-opening", PlayerID: "player-1", WalletID: "wallet-1",
		Kind: domainwallet.Opening, Origin: domainwallet.Internal, Money: mustMoney(t, "100.00", money.BRL),
		Status: domainwallet.Processed, CreatedAt: now, UpdatedAt: now,
	}
}

func TestGetTransactionUseCase_ByID_OwnerProvider_Found(t *testing.T) {
	t.Parallel()

	detail := externalDetail(t, "provider-a")
	uc := walletapp.NewGetTransactionUseCase(&fakeTransactionRepository{detailByID: detail})

	got, err := uc.ByID(context.Background(), "tx-1", walletapp.Caller{ProviderID: "provider-a"})
	if err != nil {
		t.Fatalf("ByID() error = %v", err)
	}
	if got.TransactionID != "tx-1" {
		t.Errorf("ByID() = %+v, want tx-1", got)
	}
}

func TestGetTransactionUseCase_ByID_OtherProvider_NotFound(t *testing.T) {
	t.Parallel()

	detail := externalDetail(t, "provider-a")
	uc := walletapp.NewGetTransactionUseCase(&fakeTransactionRepository{detailByID: detail})

	_, err := uc.ByID(context.Background(), "tx-1", walletapp.Caller{ProviderID: "provider-b"})
	if !errors.Is(err, operation.ErrTransactionNotFound) {
		t.Errorf("ByID() error = %v, want errors.Is(_, operation.ErrTransactionNotFound)", err)
	}
}

// TestGetTransactionUseCase_ByID_InternalOpening_NotFoundForProvider proves
// no provider - not even the wallet's own player's provider, since OPENING
// carries no providerId at all - can read the internal OPENING row (spec:
// "as ... de origem interna devolvem 404, sem revelar existência").
func TestGetTransactionUseCase_ByID_InternalOpening_NotFoundForProvider(t *testing.T) {
	t.Parallel()

	uc := walletapp.NewGetTransactionUseCase(&fakeTransactionRepository{detailByID: openingDetail(t)})

	_, err := uc.ByID(context.Background(), "tx-opening", walletapp.Caller{ProviderID: "provider-a"})
	if !errors.Is(err, operation.ErrTransactionNotFound) {
		t.Errorf("ByID() error = %v, want errors.Is(_, operation.ErrTransactionNotFound)", err)
	}
}

func TestGetTransactionUseCase_ByID_Admin_SeesOtherProviderAndOpening(t *testing.T) {
	t.Parallel()

	cases := []*walletapp.TransactionDetail{externalDetail(t, "provider-a"), openingDetail(t)}
	for _, detail := range cases {
		uc := walletapp.NewGetTransactionUseCase(&fakeTransactionRepository{detailByID: detail})
		got, err := uc.ByID(context.Background(), detail.TransactionID, walletapp.Caller{IsAdmin: true})
		if err != nil {
			t.Fatalf("ByID() for %s error = %v", detail.Kind, err)
		}
		if got.TransactionID != detail.TransactionID {
			t.Errorf("ByID() = %+v, want %+v", got, detail)
		}
	}
}

func TestGetTransactionUseCase_ByID_NotFound_MapsToOperationError(t *testing.T) {
	t.Parallel()

	uc := walletapp.NewGetTransactionUseCase(&fakeTransactionRepository{})

	_, err := uc.ByID(context.Background(), "missing", walletapp.Caller{ProviderID: "provider-a"})
	if !errors.Is(err, operation.ErrTransactionNotFound) {
		t.Errorf("ByID() error = %v, want errors.Is(_, operation.ErrTransactionNotFound)", err)
	}
}

func TestGetTransactionUseCase_ByID_UnexpectedReadFailure_PropagatesUnclassified(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset")
	uc := walletapp.NewGetTransactionUseCase(&fakeTransactionRepository{detailByIDErr: boom})

	_, err := uc.ByID(context.Background(), "tx-1", walletapp.Caller{ProviderID: "provider-a"})
	if !errors.Is(err, boom) {
		t.Errorf("ByID() error = %v, want errors.Is(_, boom)", err)
	}
}

func TestGetTransactionUseCase_ByProviderExternalID_OwnerProvider_Found(t *testing.T) {
	t.Parallel()

	detail := externalDetail(t, "provider-a")
	uc := walletapp.NewGetTransactionUseCase(&fakeTransactionRepository{detailByExternal: detail})

	got, err := uc.ByProviderExternalID(context.Background(), "provider-a", "ext-1", walletapp.Caller{ProviderID: "provider-a"})
	if err != nil {
		t.Fatalf("ByProviderExternalID() error = %v", err)
	}
	if got.TransactionID != "tx-1" {
		t.Errorf("ByProviderExternalID() = %+v, want tx-1", got)
	}
}

// TestGetTransactionUseCase_ByProviderExternalID_OtherProvider_Forbidden
// proves the mismatch is rejected before the repository is ever consulted -
// the route's own providerId governs the check (spec: "provider com
// providerId diferente devolve 403").
func TestGetTransactionUseCase_ByProviderExternalID_OtherProvider_Forbidden(t *testing.T) {
	t.Parallel()

	repo := &fakeTransactionRepository{detailByExternal: externalDetail(t, "provider-a")}
	uc := walletapp.NewGetTransactionUseCase(repo)

	_, err := uc.ByProviderExternalID(context.Background(), "provider-a", "ext-1", walletapp.Caller{ProviderID: "provider-b"})
	if !errors.Is(err, walletapp.ErrForbiddenProvider) {
		t.Errorf("ByProviderExternalID() error = %v, want errors.Is(_, walletapp.ErrForbiddenProvider)", err)
	}
}

func TestGetTransactionUseCase_ByProviderExternalID_Admin_AnyProvider(t *testing.T) {
	t.Parallel()

	detail := externalDetail(t, "provider-a")
	uc := walletapp.NewGetTransactionUseCase(&fakeTransactionRepository{detailByExternal: detail})

	got, err := uc.ByProviderExternalID(context.Background(), "provider-a", "ext-1", walletapp.Caller{IsAdmin: true})
	if err != nil {
		t.Fatalf("ByProviderExternalID() error = %v", err)
	}
	if got.TransactionID != "tx-1" {
		t.Errorf("ByProviderExternalID() = %+v, want tx-1", got)
	}
}

func TestGetTransactionUseCase_ByProviderExternalID_NotFound_MapsToOperationError(t *testing.T) {
	t.Parallel()

	uc := walletapp.NewGetTransactionUseCase(&fakeTransactionRepository{})

	_, err := uc.ByProviderExternalID(context.Background(), "provider-a", "missing", walletapp.Caller{ProviderID: "provider-a"})
	if !errors.Is(err, operation.ErrTransactionNotFound) {
		t.Errorf("ByProviderExternalID() error = %v, want errors.Is(_, operation.ErrTransactionNotFound)", err)
	}
}
