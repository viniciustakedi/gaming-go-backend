package wallet

import (
	"errors"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
)

var (
	ErrInvalidTransaction = errors.New("invalid wager transaction")
	ErrInvalidTransition  = errors.New("invalid wager transaction transition")
)

// WagerKind identifies the financial operation requested by a provider or an opening balance.
type WagerKind string

const (
	Opening  WagerKind = "OPENING"
	Bet      WagerKind = "BET"
	Win      WagerKind = "WIN"
	Loss     WagerKind = "LOSS"
	Refund   WagerKind = "REFUND"
	Rollback WagerKind = "ROLLBACK"
)

// TransactionOrigin distinguishes internal opening credits from provider operations.
type TransactionOrigin string

const (
	Internal TransactionOrigin = "INTERNAL"
	External TransactionOrigin = "EXTERNAL"
)

// TransactionStatus is the lifecycle state of a wager transaction.
type TransactionStatus string

const (
	Pending          TransactionStatus = "PENDING"
	Processed        TransactionStatus = "PROCESSED"
	Rejected         TransactionStatus = "REJECTED"
	Failed           TransactionStatus = "FAILED"
	PendingReference TransactionStatus = "PENDING_REFERENCE"
)

// ExternalTransactionInput holds all metadata supplied by an external provider operation.
type ExternalTransactionInput struct {
	ID                             string
	ExternalTransactionID          string
	ProviderID                     string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           WagerKind
	Money                          money.Money
	ReferenceExternalTransactionID string
	// ReferenceTransactionID is the internal id the reference resolved to.
	// Empty for BET, LOSS and a WIN with no reference: none resolve one.
	ReferenceTransactionID string
	CreatedAt              time.Time
}

// OpeningTransactionInput contains only the metadata that belongs to an internal opening credit.
type OpeningTransactionInput struct {
	ID        string
	WalletID  string
	PlayerID  string
	Money     money.Money
	CreatedAt time.Time
}

// RehydratedTransaction contains persisted transaction state. It is validated without a transition.
type RehydratedTransaction struct {
	ID                             string
	ExternalTransactionID          string
	ProviderID                     string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           WagerKind
	Origin                         TransactionOrigin
	Money                          money.Money
	ReferenceExternalTransactionID string
	ReferenceTransactionID         string
	Status                         TransactionStatus
	FailureCode                    string
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
}

// WagerTransaction has an encapsulated lifecycle and immutable request metadata.
type WagerTransaction struct {
	id                             string
	externalTransactionID          string
	providerID                     string
	idempotencyKey                 string
	payloadHash                    string
	walletID                       string
	playerID                       string
	roundID                        string
	gameID                         string
	kind                           WagerKind
	origin                         TransactionOrigin
	money                          money.Money
	referenceExternalTransactionID string
	referenceTransactionID         string
	status                         TransactionStatus
	failureCode                    string
	createdAt                      time.Time
	updatedAt                      time.Time
}

// NewExternalTransaction constructs a pending provider operation with all required external metadata.
func NewExternalTransaction(input ExternalTransactionInput) (*WagerTransaction, error) {
	transaction := &WagerTransaction{
		id: input.ID, externalTransactionID: input.ExternalTransactionID, providerID: input.ProviderID,
		idempotencyKey: input.IdempotencyKey, payloadHash: input.PayloadHash, walletID: input.WalletID,
		playerID: input.PlayerID, roundID: input.RoundID, gameID: input.GameID, kind: input.Kind,
		origin: External, money: input.Money, referenceExternalTransactionID: input.ReferenceExternalTransactionID,
		referenceTransactionID: input.ReferenceTransactionID,
		status:                 Pending, createdAt: input.CreatedAt.UTC(), updatedAt: input.CreatedAt.UTC(),
	}
	if err := transaction.validate(); err != nil {
		return nil, err
	}
	return transaction, nil
}

// NewOpeningTransaction constructs a pending internal opening transaction without external metadata.
func NewOpeningTransaction(input OpeningTransactionInput) (*WagerTransaction, error) {
	transaction := &WagerTransaction{id: input.ID, walletID: input.WalletID, playerID: input.PlayerID, kind: Opening, origin: Internal, money: input.Money, status: Pending, createdAt: input.CreatedAt.UTC(), updatedAt: input.CreatedAt.UTC()}
	if err := transaction.validate(); err != nil {
		return nil, err
	}
	return transaction, nil
}

// RehydrateTransaction reconstructs persisted state without changing its lifecycle.
func RehydrateTransaction(input RehydratedTransaction) (*WagerTransaction, error) {
	transaction := &WagerTransaction{
		id: input.ID, externalTransactionID: input.ExternalTransactionID, providerID: input.ProviderID,
		idempotencyKey: input.IdempotencyKey, payloadHash: input.PayloadHash, walletID: input.WalletID,
		playerID: input.PlayerID, roundID: input.RoundID, gameID: input.GameID, kind: input.Kind, origin: input.Origin,
		money: input.Money, referenceExternalTransactionID: input.ReferenceExternalTransactionID,
		referenceTransactionID: input.ReferenceTransactionID, status: input.Status, failureCode: input.FailureCode,
		createdAt: input.CreatedAt.UTC(), updatedAt: input.UpdatedAt.UTC(),
	}
	if err := transaction.validate(); err != nil {
		return nil, err
	}
	return transaction, nil
}

// MarkProcessed completes a pending transaction or one waiting for a reference.
func (t *WagerTransaction) MarkProcessed(at time.Time) error { return t.transition(Processed, "", at) }

// MarkRejected completes a transaction with a durable business failure code.
func (t *WagerTransaction) MarkRejected(code string, at time.Time) error {
	return t.transition(Rejected, code, at)
}

// MarkFailed completes a transaction with a durable permanent failure code.
func (t *WagerTransaction) MarkFailed(code string, at time.Time) error {
	return t.transition(Failed, code, at)
}

// MarkPendingReference records that a pending transaction must wait for a reference.
func (t *WagerTransaction) MarkPendingReference(at time.Time) error {
	return t.transition(PendingReference, "", at)
}

// ResolveReference records the immutable internal identity once a pending
// operation finds the external reference it was waiting for. It is confined
// to PENDING_REFERENCE so a terminal audit record can never be retargeted.
func (t *WagerTransaction) ResolveReference(referenceTransactionID string) error {
	if t == nil || t.status != PendingReference || referenceTransactionID == "" || t.referenceTransactionID != "" {
		return transactionError(ErrInvalidTransition)
	}
	t.referenceTransactionID = referenceTransactionID
	return nil
}

func (t *WagerTransaction) transition(next TransactionStatus, failureCode string, at time.Time) error {
	if t == nil || at.IsZero() {
		return transactionError(ErrInvalidTransition)
	}
	if !isAllowedTransition(t.status, next) {
		return transactionError(ErrInvalidTransition)
	}
	if (next == Rejected || next == Failed) && failureCode == "" {
		return transactionError(ErrInvalidTransaction)
	}
	if (next == Processed || next == PendingReference) && failureCode != "" {
		return transactionError(ErrInvalidTransaction)
	}
	t.status, t.failureCode, t.updatedAt = next, failureCode, at.UTC()
	return nil
}

func isAllowedTransition(from, to TransactionStatus) bool {
	if from == Pending {
		return to == Processed || to == Rejected || to == Failed || to == PendingReference
	}
	return from == PendingReference && (to == Processed || to == Rejected || to == Failed)
}

func (t *WagerTransaction) validate() error {
	if t == nil || t.id == "" || t.walletID == "" || t.playerID == "" || t.createdAt.IsZero() || t.updatedAt.IsZero() || !isKind(t.kind) || !isStatus(t.status) {
		return transactionError(ErrInvalidTransaction)
	}
	if err := validateNonNegativeMoney(t.money); err != nil {
		return err
	}
	if t.origin == Internal {
		if t.kind != Opening || t.externalTransactionID != "" || t.providerID != "" || t.idempotencyKey != "" || t.payloadHash != "" || t.roundID != "" || t.gameID != "" || t.referenceExternalTransactionID != "" || t.referenceTransactionID != "" {
			return transactionError(ErrInvalidTransaction)
		}
	} else if t.origin == External {
		if t.kind == Opening || t.externalTransactionID == "" || t.providerID == "" || t.idempotencyKey == "" || t.payloadHash == "" || t.roundID == "" || t.gameID == "" {
			return transactionError(ErrInvalidTransaction)
		}
	} else {
		return transactionError(ErrInvalidTransaction)
	}
	if (t.kind == Refund || t.kind == Rollback) && t.referenceExternalTransactionID == "" {
		return transactionError(ErrInvalidTransaction)
	}
	if (t.status == Rejected || t.status == Failed) != (t.failureCode != "") {
		return transactionError(ErrInvalidTransaction)
	}
	if (t.status == Pending || t.status == PendingReference || t.status == Processed) && t.failureCode != "" {
		return transactionError(ErrInvalidTransaction)
	}
	return nil
}

func validateNonNegativeMoney(value money.Money) error {
	currency, err := value.Currency()
	if err != nil {
		return transactionError(ErrInvalidTransaction)
	}
	zero, _ := money.Zero(currency)
	comparison, err := value.Compare(zero)
	if err != nil {
		return transactionError(ErrInvalidTransaction)
	}
	if comparison < 0 {
		return transactionError(ErrInvalidTransaction)
	}
	return nil
}

func isKind(kind WagerKind) bool {
	return kind == Opening || kind == Bet || kind == Win || kind == Loss || kind == Refund || kind == Rollback
}

func isStatus(status TransactionStatus) bool {
	return status == Pending || status == Processed || status == Rejected || status == Failed || status == PendingReference
}

// ID returns the transaction identifier.
func (t *WagerTransaction) ID() string {
	if t == nil {
		return ""
	}
	return t.id
}

// ExternalTransactionID returns the provider transaction identifier.
func (t *WagerTransaction) ExternalTransactionID() string {
	if t == nil {
		return ""
	}
	return t.externalTransactionID
}

// ProviderID returns the provider identifier.
func (t *WagerTransaction) ProviderID() string {
	if t == nil {
		return ""
	}
	return t.providerID
}

// IdempotencyKey returns the provider idempotency key.
func (t *WagerTransaction) IdempotencyKey() string {
	if t == nil {
		return ""
	}
	return t.idempotencyKey
}

// PayloadHash returns the canonical external payload hash.
func (t *WagerTransaction) PayloadHash() string {
	if t == nil {
		return ""
	}
	return t.payloadHash
}

// WalletID returns the target wallet identifier.
func (t *WagerTransaction) WalletID() string {
	if t == nil {
		return ""
	}
	return t.walletID
}

// PlayerID returns the target player identifier.
func (t *WagerTransaction) PlayerID() string {
	if t == nil {
		return ""
	}
	return t.playerID
}

// RoundID returns the provider round identifier.
func (t *WagerTransaction) RoundID() string {
	if t == nil {
		return ""
	}
	return t.roundID
}

// GameID returns the provider game identifier.
func (t *WagerTransaction) GameID() string {
	if t == nil {
		return ""
	}
	return t.gameID
}

// Kind returns the financial operation kind.
func (t *WagerTransaction) Kind() WagerKind {
	if t == nil {
		return ""
	}
	return t.kind
}

// Origin returns whether the transaction is internal or external.
func (t *WagerTransaction) Origin() TransactionOrigin {
	if t == nil {
		return ""
	}
	return t.origin
}

// Money returns the transaction amount.
func (t *WagerTransaction) Money() money.Money {
	if t == nil {
		return money.Money{}
	}
	return t.money
}

// ReferenceExternalTransactionID returns the provider reference identifier.
func (t *WagerTransaction) ReferenceExternalTransactionID() string {
	if t == nil {
		return ""
	}
	return t.referenceExternalTransactionID
}

// ReferenceTransactionID returns the resolved internal reference identifier.
func (t *WagerTransaction) ReferenceTransactionID() string {
	if t == nil {
		return ""
	}
	return t.referenceTransactionID
}

// Status returns the current transaction lifecycle state.
func (t *WagerTransaction) Status() TransactionStatus {
	if t == nil {
		return ""
	}
	return t.status
}

// FailureCode returns the durable terminal failure code.
func (t *WagerTransaction) FailureCode() string {
	if t == nil {
		return ""
	}
	return t.failureCode
}

// CreatedAt returns when the transaction was created.
func (t *WagerTransaction) CreatedAt() time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.createdAt
}

// UpdatedAt returns when the transaction was last changed.
func (t *WagerTransaction) UpdatedAt() time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.updatedAt
}

func transactionError(kind error) error { return &Error{Kind: kind} }
