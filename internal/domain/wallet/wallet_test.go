package wallet_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

var occurredAt = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

func TestOpenWalletCreatesOpeningCreditWithoutChangingVersion(t *testing.T) {
	t.Parallel()

	walletValue, entry, err := wallet.Open(wallet.OpenInput{
		ID:                   "wallet-1",
		PlayerID:             "player-1",
		Currency:             money.BRL,
		InitialBalance:       mustMoney(t, "100.00", money.BRL),
		OpeningTransactionID: "transaction-1",
		LedgerEntryID:        "entry-1",
		OccurredAt:           occurredAt,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if got := walletValue.Version(); got != 1 {
		t.Errorf("Version() = %d, want 1", got)
	}
	if got := decimal(t, walletValue.Balance()); got != "100.00" {
		t.Errorf("Balance() = %s, want 100.00", got)
	}
	if got := entry.Direction(); got != wallet.Credit {
		t.Errorf("Direction() = %s, want CREDIT", got)
	}
	if got := decimal(t, entry.BalanceAfter()); got != "100.00" {
		t.Errorf("BalanceAfter() = %s, want 100.00", got)
	}
}

func TestNewAndRehydratedWalletDoNotCreateMovements(t *testing.T) {
	t.Parallel()

	created, err := wallet.New(wallet.NewInput{ID: "wallet-1", PlayerID: "player-1", Currency: money.BRL, OccurredAt: occurredAt})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := decimal(t, created.Balance()); got != "0.00" {
		t.Errorf("new balance = %s, want 0.00", got)
	}
	if created.Version() != 1 {
		t.Errorf("new version = %d, want 1", created.Version())
	}

	rehydrated, err := wallet.Rehydrate(wallet.RehydratedWallet{ID: "wallet-1", PlayerID: "player-1", Currency: money.BRL, Balance: mustMoney(t, "20.00", money.BRL), Version: 4, CreatedAt: occurredAt, UpdatedAt: occurredAt.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Rehydrate() error = %v", err)
	}
	if got := decimal(t, rehydrated.Balance()); got != "20.00" {
		t.Errorf("rehydrated balance = %s, want 20.00", got)
	}
	if rehydrated.Version() != 4 {
		t.Errorf("rehydrated version = %d, want 4", rehydrated.Version())
	}
}

func TestRehydrateWalletRejectsInvalidPersistedState(t *testing.T) {
	t.Parallel()
	negativeBalance, err := money.New(-1, money.BRL)
	if err != nil {
		t.Fatalf("money.New() error = %v", err)
	}
	tests := []struct {
		name  string
		input wallet.RehydratedWallet
		want  error
	}{
		{name: "version zero", input: wallet.RehydratedWallet{ID: "wallet-1", PlayerID: "player-1", Currency: money.BRL, Balance: mustMoney(t, "20.00", money.BRL), Version: 0, CreatedAt: occurredAt, UpdatedAt: occurredAt}, want: wallet.ErrInvalidWallet},
		{name: "negative balance", input: wallet.RehydratedWallet{ID: "wallet-1", PlayerID: "player-1", Currency: money.BRL, Balance: negativeBalance, Version: 1, CreatedAt: occurredAt, UpdatedAt: occurredAt}, want: wallet.ErrInvalidWallet},
		{name: "balance currency mismatch", input: wallet.RehydratedWallet{ID: "wallet-1", PlayerID: "player-1", Currency: money.BRL, Balance: mustMoney(t, "20.00", money.USD), Version: 1, CreatedAt: occurredAt, UpdatedAt: occurredAt}, want: wallet.ErrCurrencyMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := wallet.Rehydrate(tt.input); !errors.Is(err, tt.want) {
				t.Errorf("Rehydrate() error = %v, want errors.Is(_, %v)", err, tt.want)
			}
		})
	}
}

func TestWalletDebitAndCreditMoveBalanceAndVersion(t *testing.T) {
	t.Parallel()

	walletValue, _, err := wallet.Open(openInput(t, "100.00"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	debit, err := walletValue.Debit(wallet.MovementInput{LedgerEntryID: "entry-2", TransactionID: "transaction-2", Money: mustMoney(t, "80.00", money.BRL), OccurredAt: occurredAt.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Debit() error = %v", err)
	}
	if got := decimal(t, debit.BalanceAfter()); got != "20.00" {
		t.Errorf("debit balance after = %s, want 20.00", got)
	}
	credit, err := walletValue.Credit(wallet.MovementInput{LedgerEntryID: "entry-3", TransactionID: "transaction-3", Money: mustMoney(t, "5.00", money.BRL), OccurredAt: occurredAt.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("Credit() error = %v", err)
	}
	if got := decimal(t, credit.BalanceAfter()); got != "25.00" {
		t.Errorf("credit balance after = %s, want 25.00", got)
	}
	if got := walletValue.Version(); got != 3 {
		t.Errorf("Version() = %d, want 3", got)
	}
}

func TestWalletDebitRejectsInsufficientFunds(t *testing.T) {
	t.Parallel()
	walletValue, _, err := wallet.Open(openInput(t, "10.00"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	_, err = walletValue.Debit(wallet.MovementInput{LedgerEntryID: "entry-2", TransactionID: "transaction-2", Money: mustMoney(t, "10.01", money.BRL), OccurredAt: occurredAt.Add(time.Minute)})
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Errorf("Debit() error = %v, want errors.Is(_, ErrInsufficientFunds)", err)
	}
}

func TestWalletMovementRejectsDifferentCurrency(t *testing.T) {
	t.Parallel()
	walletValue, _, err := wallet.Open(openInput(t, "10.00"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	_, err = walletValue.Credit(wallet.MovementInput{LedgerEntryID: "entry-2", TransactionID: "transaction-2", Money: mustMoney(t, "1.00", money.USD), OccurredAt: occurredAt.Add(time.Minute)})
	if !errors.Is(err, wallet.ErrCurrencyMismatch) {
		t.Errorf("Credit() error = %v, want errors.Is(_, ErrCurrencyMismatch)", err)
	}
}

func TestWalletLedgerEntryRejectsIncoherentBalance(t *testing.T) {
	t.Parallel()
	negativeBalance, err := money.New(-1, money.BRL)
	if err != nil {
		t.Fatalf("money.New() error = %v", err)
	}
	tests := []struct {
		name  string
		input wallet.LedgerEntryInput
	}{
		{name: "debit balance equation", input: wallet.LedgerEntryInput{ID: "entry-1", WalletID: "wallet-1", TransactionID: "transaction-1", Direction: wallet.Debit, Money: mustMoney(t, "20.00", money.BRL), BalanceBefore: mustMoney(t, "100.00", money.BRL), BalanceAfter: mustMoney(t, "90.00", money.BRL), OccurredAt: occurredAt}},
		{name: "credit balance equation", input: wallet.LedgerEntryInput{ID: "entry-1", WalletID: "wallet-1", TransactionID: "transaction-1", Direction: wallet.Credit, Money: mustMoney(t, "20.00", money.BRL), BalanceBefore: mustMoney(t, "100.00", money.BRL), BalanceAfter: mustMoney(t, "110.00", money.BRL), OccurredAt: occurredAt}},
		{name: "negative balance after", input: wallet.LedgerEntryInput{ID: "entry-1", WalletID: "wallet-1", TransactionID: "transaction-1", Direction: wallet.Debit, Money: mustMoney(t, "20.00", money.BRL), BalanceBefore: mustMoney(t, "10.00", money.BRL), BalanceAfter: negativeBalance, OccurredAt: occurredAt}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := wallet.NewWalletLedgerEntry(tt.input); !errors.Is(err, wallet.ErrInvalidLedgerEntry) {
				t.Errorf("NewWalletLedgerEntry() error = %v, want errors.Is(_, ErrInvalidLedgerEntry)", err)
			}
		})
	}
}

func openInput(t *testing.T, amount string) wallet.OpenInput {
	t.Helper()
	return wallet.OpenInput{ID: "wallet-1", PlayerID: "player-1", Currency: money.BRL, InitialBalance: mustMoney(t, amount, money.BRL), OpeningTransactionID: "transaction-1", LedgerEntryID: "entry-1", OccurredAt: occurredAt}
}

func mustMoney(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	value, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", amount, err)
	}
	return value
}

func decimal(t *testing.T, value money.Money) string {
	t.Helper()
	encoded, err := value.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}
	var wire struct {
		Amount string `json:"amount"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	return wire.Amount
}
