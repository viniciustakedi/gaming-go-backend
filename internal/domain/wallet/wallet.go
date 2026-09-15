// Package wallet contains the pure financial domain aggregate and its ledger.
package wallet

import (
	"errors"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
)

var (
	ErrInvalidWallet      = errors.New("invalid wallet")
	ErrInsufficientFunds  = errors.New("insufficient funds")
	ErrCurrencyMismatch   = errors.New("wallet currency mismatch")
	ErrInvalidLedgerEntry = errors.New("invalid wallet ledger entry")
)

// Error identifies a wallet-domain failure that callers can classify with errors.Is.
type Error struct{ Kind error }

// Error returns the category message.
func (e *Error) Error() string { return e.Kind.Error() }

// Unwrap returns the category for errors.Is matching.
func (e *Error) Unwrap() error { return e.Kind }

// Direction tells whether a ledger entry adds to or removes from a wallet balance.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// Wallet is the aggregate root for one player's balance in one currency.
type Wallet struct {
	id        string
	playerID  string
	currency  money.Currency
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// NewInput contains the data required to create an empty wallet.
type NewInput struct {
	ID         string
	PlayerID   string
	Currency   money.Currency
	OccurredAt time.Time
}

// OpenInput contains the data required to create a wallet and its optional opening credit.
type OpenInput struct {
	ID                   string
	PlayerID             string
	Currency             money.Currency
	InitialBalance       money.Money
	OpeningTransactionID string
	LedgerEntryID        string
	OccurredAt           time.Time
}

// RehydratedWallet contains persisted state that must be validated without applying a movement.
type RehydratedWallet struct {
	ID        string
	PlayerID  string
	Currency  money.Currency
	Balance   money.Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MovementInput identifies a balance movement made through the aggregate.
type MovementInput struct {
	LedgerEntryID string
	TransactionID string
	Money         money.Money
	OccurredAt    time.Time
}

// New creates an empty wallet at version one.
func New(input NewInput) (*Wallet, error) {
	if input.ID == "" || input.PlayerID == "" || input.OccurredAt.IsZero() {
		return nil, walletError(ErrInvalidWallet)
	}
	balance, err := money.Zero(input.Currency)
	if err != nil {
		return nil, walletError(ErrInvalidWallet)
	}
	now := input.OccurredAt.UTC()
	return &Wallet{id: input.ID, playerID: input.PlayerID, currency: input.Currency, balance: balance, version: 1, createdAt: now, updatedAt: now}, nil
}

// Open creates a wallet and, for a positive initial balance, its opening credit without changing version.
func Open(input OpenInput) (*Wallet, *WalletLedgerEntry, error) {
	walletValue, err := New(NewInput{ID: input.ID, PlayerID: input.PlayerID, Currency: input.Currency, OccurredAt: input.OccurredAt})
	if err != nil {
		return nil, nil, err
	}
	if err := requireCurrency(input.InitialBalance, input.Currency); err != nil {
		return nil, nil, err
	}
	zero, _ := money.Zero(input.Currency)
	comparison, err := input.InitialBalance.Compare(zero)
	if err != nil || comparison < 0 {
		return nil, nil, walletError(ErrInvalidWallet)
	}
	if comparison == 0 {
		return walletValue, nil, nil
	}
	if input.OpeningTransactionID == "" || input.LedgerEntryID == "" {
		return nil, nil, walletError(ErrInvalidWallet)
	}
	entry, err := NewWalletLedgerEntry(LedgerEntryInput{
		ID: input.LedgerEntryID, WalletID: input.ID, TransactionID: input.OpeningTransactionID, Direction: Credit,
		Money: input.InitialBalance, BalanceBefore: zero, BalanceAfter: input.InitialBalance, OccurredAt: input.OccurredAt,
	})
	if err != nil {
		return nil, nil, err
	}
	walletValue.balance = input.InitialBalance
	return walletValue, entry, nil
}

// Rehydrate reconstructs a wallet from persisted state without changing it.
func Rehydrate(input RehydratedWallet) (*Wallet, error) {
	if input.ID == "" || input.PlayerID == "" || input.Version < 1 || input.CreatedAt.IsZero() || input.UpdatedAt.IsZero() {
		return nil, walletError(ErrInvalidWallet)
	}
	if err := requireCurrency(input.Balance, input.Currency); err != nil {
		return nil, err
	}
	zero, _ := money.Zero(input.Currency)
	comparison, err := input.Balance.Compare(zero)
	if err != nil || comparison < 0 {
		return nil, walletError(ErrInvalidWallet)
	}
	return &Wallet{id: input.ID, playerID: input.PlayerID, currency: input.Currency, balance: input.Balance, version: input.Version, createdAt: input.CreatedAt.UTC(), updatedAt: input.UpdatedAt.UTC()}, nil
}

// Debit removes money from the balance and produces its immutable audit entry.
func (w *Wallet) Debit(input MovementInput) (*WalletLedgerEntry, error) {
	return w.move(Debit, input)
}

// Credit adds money to the balance and produces its immutable audit entry.
func (w *Wallet) Credit(input MovementInput) (*WalletLedgerEntry, error) {
	return w.move(Credit, input)
}

func (w *Wallet) move(direction Direction, input MovementInput) (*WalletLedgerEntry, error) {
	if w == nil || input.LedgerEntryID == "" || input.TransactionID == "" || input.OccurredAt.IsZero() {
		return nil, walletError(ErrInvalidWallet)
	}
	if err := requireCurrency(input.Money, w.currency); err != nil {
		return nil, err
	}
	zero, _ := money.Zero(w.currency)
	amountComparison, err := input.Money.Compare(zero)
	if err != nil || amountComparison <= 0 {
		return nil, walletError(ErrInvalidLedgerEntry)
	}
	var after money.Money
	if direction == Debit {
		if balanceComparison, compareErr := w.balance.Compare(input.Money); compareErr != nil || balanceComparison < 0 {
			return nil, walletError(ErrInsufficientFunds)
		}
		after, err = w.balance.Subtract(input.Money)
	} else {
		after, err = w.balance.Add(input.Money)
	}
	if err != nil {
		return nil, err
	}
	entry, err := NewWalletLedgerEntry(LedgerEntryInput{ID: input.LedgerEntryID, WalletID: w.id, TransactionID: input.TransactionID, Direction: direction, Money: input.Money, BalanceBefore: w.balance, BalanceAfter: after, OccurredAt: input.OccurredAt})
	if err != nil {
		return nil, err
	}
	w.balance, w.version, w.updatedAt = after, w.version+1, input.OccurredAt.UTC()
	return entry, nil
}

// ID returns the wallet identifier.
func (w *Wallet) ID() string {
	if w == nil {
		return ""
	}
	return w.id
}

// PlayerID returns the owning player identifier.
func (w *Wallet) PlayerID() string {
	if w == nil {
		return ""
	}
	return w.playerID
}

// Currency returns the wallet currency.
func (w *Wallet) Currency() money.Currency {
	if w == nil {
		return ""
	}
	return w.currency
}

// Balance returns the current wallet balance.
func (w *Wallet) Balance() money.Money {
	if w == nil {
		return money.Money{}
	}
	return w.balance
}

// Version returns the optimistic concurrency version.
func (w *Wallet) Version() int64 {
	if w == nil {
		return 0
	}
	return w.version
}

// CreatedAt returns when the wallet was created.
func (w *Wallet) CreatedAt() time.Time {
	if w == nil {
		return time.Time{}
	}
	return w.createdAt
}

// UpdatedAt returns when the wallet was last changed.
func (w *Wallet) UpdatedAt() time.Time {
	if w == nil {
		return time.Time{}
	}
	return w.updatedAt
}

// LedgerEntryInput is the complete immutable representation of a ledger entry.
type LedgerEntryInput struct {
	ID            string
	WalletID      string
	TransactionID string
	Direction     Direction
	Money         money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	OccurredAt    time.Time
}

// WalletLedgerEntry is an immutable movement audited against adjacent balances.
type WalletLedgerEntry struct{ input LedgerEntryInput }

// NewWalletLedgerEntry validates the balance equation and constructs an immutable entry.
func NewWalletLedgerEntry(input LedgerEntryInput) (*WalletLedgerEntry, error) {
	if input.ID == "" || input.WalletID == "" || input.TransactionID == "" || input.OccurredAt.IsZero() || (input.Direction != Debit && input.Direction != Credit) {
		return nil, walletError(ErrInvalidLedgerEntry)
	}
	currency, err := input.Money.Currency()
	if err != nil {
		return nil, walletError(ErrInvalidLedgerEntry)
	}
	if err := requireCurrency(input.BalanceBefore, currency); err != nil {
		return nil, walletError(ErrInvalidLedgerEntry)
	}
	if err := requireCurrency(input.BalanceAfter, currency); err != nil {
		return nil, walletError(ErrInvalidLedgerEntry)
	}
	zero, _ := money.Zero(currency)
	amountComparison, err := input.Money.Compare(zero)
	if err != nil || amountComparison <= 0 {
		return nil, walletError(ErrInvalidLedgerEntry)
	}
	beforeComparison, err := input.BalanceBefore.Compare(zero)
	if err != nil || beforeComparison < 0 {
		return nil, walletError(ErrInvalidLedgerEntry)
	}
	afterComparison, err := input.BalanceAfter.Compare(zero)
	if err != nil || afterComparison < 0 {
		return nil, walletError(ErrInvalidLedgerEntry)
	}
	expected := input.BalanceBefore
	if input.Direction == Debit {
		expected, err = expected.Subtract(input.Money)
	} else {
		expected, err = expected.Add(input.Money)
	}
	if err != nil {
		return nil, walletError(ErrInvalidLedgerEntry)
	}
	comparison, err := expected.Compare(input.BalanceAfter)
	if err != nil || comparison != 0 {
		return nil, walletError(ErrInvalidLedgerEntry)
	}
	input.OccurredAt = input.OccurredAt.UTC()
	return &WalletLedgerEntry{input: input}, nil
}

// ID returns the ledger entry identifier.
func (e *WalletLedgerEntry) ID() string {
	if e == nil {
		return ""
	}
	return e.input.ID
}

// WalletID returns the ledger entry wallet identifier.
func (e *WalletLedgerEntry) WalletID() string {
	if e == nil {
		return ""
	}
	return e.input.WalletID
}

// TransactionID returns the ledger entry transaction identifier.
func (e *WalletLedgerEntry) TransactionID() string {
	if e == nil {
		return ""
	}
	return e.input.TransactionID
}

// Direction returns whether the entry debits or credits the wallet.
func (e *WalletLedgerEntry) Direction() Direction {
	if e == nil {
		return ""
	}
	return e.input.Direction
}

// Money returns the entry amount.
func (e *WalletLedgerEntry) Money() money.Money {
	if e == nil {
		return money.Money{}
	}
	return e.input.Money
}

// BalanceBefore returns the balance immediately before the entry.
func (e *WalletLedgerEntry) BalanceBefore() money.Money {
	if e == nil {
		return money.Money{}
	}
	return e.input.BalanceBefore
}

// BalanceAfter returns the balance immediately after the entry.
func (e *WalletLedgerEntry) BalanceAfter() money.Money {
	if e == nil {
		return money.Money{}
	}
	return e.input.BalanceAfter
}

// OccurredAt returns when the entry was recorded.
func (e *WalletLedgerEntry) OccurredAt() time.Time {
	if e == nil {
		return time.Time{}
	}
	return e.input.OccurredAt
}

func requireCurrency(value money.Money, currency money.Currency) error {
	actual, err := value.Currency()
	if err != nil {
		return walletError(ErrInvalidWallet)
	}
	if actual != currency {
		return walletError(ErrCurrencyMismatch)
	}
	return nil
}

func walletError(kind error) error { return &Error{Kind: kind} }
