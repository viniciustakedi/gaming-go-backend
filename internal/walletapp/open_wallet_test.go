package walletapp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

func mustMoney(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	value, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("money.Parse(%q, %q) error = %v", amount, currency, err)
	}
	return value
}

func TestOpenWalletUseCase_ZeroBalance_CreatesOnlyTheWallet(t *testing.T) {
	t.Parallel()

	wallets := &fakeWalletRepository{}
	transactions := &fakeTransactionRepository{}
	ledger := &fakeLedgerRepository{}
	outbox := &fakeOutboxRepository{}
	uc := walletapp.NewOpenWalletUseCase(&fakeUnitOfWork{wallets: wallets, transactions: transactions, ledger: ledger, outbox: outbox})

	walletValue, err := uc.Open(context.Background(), walletapp.OpenWalletInput{
		PlayerID: "player-1", InitialBalance: mustMoney(t, "0.00", money.BRL), CorrelationID: "corr-1",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if walletValue.Version() != 1 {
		t.Errorf("Version() = %d, want 1", walletValue.Version())
	}
	balance, err := walletValue.Balance().MinorUnits()
	if err != nil || balance != 0 {
		t.Errorf("Balance().MinorUnits() = (%d, %v), want (0, nil)", balance, err)
	}
	if len(wallets.inserted) != 1 {
		t.Fatalf("wallets inserted = %d, want 1", len(wallets.inserted))
	}
	if len(transactions.inserted) != 0 {
		t.Errorf("transactions inserted = %d, want 0 for a zero-balance opening", len(transactions.inserted))
	}
	if len(ledger.inserted) != 0 {
		t.Errorf("ledger entries inserted = %d, want 0 for a zero-balance opening", len(ledger.inserted))
	}
	if len(outbox.inserted) != 0 {
		t.Errorf("outbox records inserted = %d, want 0 for a zero-balance opening", len(outbox.inserted))
	}
}

func TestOpenWalletUseCase_PositiveBalance_RecordsOpeningLedgerAndOutbox(t *testing.T) {
	t.Parallel()

	wallets := &fakeWalletRepository{}
	transactions := &fakeTransactionRepository{}
	ledger := &fakeLedgerRepository{}
	outbox := &fakeOutboxRepository{}
	uc := walletapp.NewOpenWalletUseCase(&fakeUnitOfWork{wallets: wallets, transactions: transactions, ledger: ledger, outbox: outbox})

	walletValue, err := uc.Open(context.Background(), walletapp.OpenWalletInput{
		PlayerID: "player-1", InitialBalance: mustMoney(t, "100.00", money.BRL), CorrelationID: "corr-1",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if walletValue.Version() != 1 {
		t.Errorf("Version() = %d, want 1 - opening credits the balance without incrementing version", walletValue.Version())
	}
	balance, err := walletValue.Balance().MinorUnits()
	if err != nil || balance != 10000 {
		t.Errorf("Balance().MinorUnits() = (%d, %v), want (10000, nil)", balance, err)
	}

	if len(transactions.inserted) != 1 {
		t.Fatalf("transactions inserted = %d, want 1", len(transactions.inserted))
	}
	tx := transactions.inserted[0]
	if tx.Kind() != domainwallet.Opening || tx.Origin() != domainwallet.Internal || tx.Status() != domainwallet.Processed {
		t.Errorf("opening transaction = kind %q origin %q status %q, want OPENING/INTERNAL/PROCESSED", tx.Kind(), tx.Origin(), tx.Status())
	}
	if transactions.balances[0] == nil || *transactions.balances[0] != 10000 {
		t.Errorf("resulting balance recorded = %v, want 10000", transactions.balances[0])
	}

	if len(ledger.inserted) != 1 {
		t.Fatalf("ledger entries inserted = %d, want 1", len(ledger.inserted))
	}
	entry := ledger.inserted[0]
	if entry.Direction() != domainwallet.Credit {
		t.Errorf("ledger entry direction = %q, want CREDIT", entry.Direction())
	}
	amount, _ := entry.Money().MinorUnits()
	if amount != 10000 {
		t.Errorf("ledger entry amount = %d, want 10000", amount)
	}

	if len(outbox.inserted) != 2 {
		t.Fatalf("outbox records inserted = %d, want 2", len(outbox.inserted))
	}
	if outbox.inserted[0].eventType != domainwallet.WagerTransactionProcessedEventType || outbox.inserted[0].aggregateType != "WagerTransaction" {
		t.Errorf("first outbox record = %+v, want WagerTransactionProcessed/WagerTransaction", outbox.inserted[0])
	}
	if outbox.inserted[1].eventType != domainwallet.WalletBalanceChangedEventType || outbox.inserted[1].aggregateType != "Wallet" {
		t.Errorf("second outbox record = %+v, want WalletBalanceChanged/Wallet", outbox.inserted[1])
	}
}

func TestOpenWalletUseCase_AlreadyExists_MapsToConflict(t *testing.T) {
	t.Parallel()

	wallets := &fakeWalletRepository{insertErr: walletapp.ErrAlreadyExists}
	uc := walletapp.NewOpenWalletUseCase(&fakeUnitOfWork{wallets: wallets, transactions: &fakeTransactionRepository{}, ledger: &fakeLedgerRepository{}, outbox: &fakeOutboxRepository{}})

	_, err := uc.Open(context.Background(), walletapp.OpenWalletInput{PlayerID: "player-1", InitialBalance: mustMoney(t, "0.00", money.BRL)})
	if !errors.Is(err, operation.ErrWalletAlreadyExists) {
		t.Errorf("Open() error = %v, want errors.Is(_, operation.ErrWalletAlreadyExists)", err)
	}
}

func TestOpenWalletUseCase_UnexpectedWriteFailure_PropagatesUnclassified(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset")
	wallets := &fakeWalletRepository{insertErr: boom}
	uc := walletapp.NewOpenWalletUseCase(&fakeUnitOfWork{wallets: wallets, transactions: &fakeTransactionRepository{}, ledger: &fakeLedgerRepository{}, outbox: &fakeOutboxRepository{}})

	_, err := uc.Open(context.Background(), walletapp.OpenWalletInput{PlayerID: "player-1", InitialBalance: mustMoney(t, "0.00", money.BRL)})
	if !errors.Is(err, boom) {
		t.Errorf("Open() error = %v, want errors.Is(_, boom) - the caller classifies an unrecognized failure as transient", err)
	}
	var opErr *operation.Error
	if errors.As(err, &opErr) {
		t.Errorf("Open() error = %v, want it NOT to be an *operation.Error - only recognized business outcomes are", err)
	}
}

func TestOpenWalletUseCase_InvalidMoney_MapsToInvalidMoney(t *testing.T) {
	t.Parallel()

	uc := walletapp.NewOpenWalletUseCase(&fakeUnitOfWork{wallets: &fakeWalletRepository{}, transactions: &fakeTransactionRepository{}, ledger: &fakeLedgerRepository{}, outbox: &fakeOutboxRepository{}})

	_, err := uc.Open(context.Background(), walletapp.OpenWalletInput{PlayerID: "player-1", InitialBalance: money.Money{}})
	if !errors.Is(err, operation.ErrInvalidMoney) {
		t.Errorf("Open() error = %v, want errors.Is(_, operation.ErrInvalidMoney)", err)
	}
}
