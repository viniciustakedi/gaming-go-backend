package wallet

import (
	"errors"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
)

var ErrInvalidEvent = errors.New("invalid domain event")

const (
	WagerTransactionProcessedEventType        = "WagerTransactionProcessed"
	WagerTransactionRejectedEventType         = "WagerTransactionRejected"
	WalletBalanceChangedEventType             = "WalletBalanceChanged"
	WagerTransactionPendingReferenceEventType = "WagerTransactionPendingReference"
	eventVersion                              = 1
)

// EventMetadata is shared envelope metadata supplied by the application at commit time.
type EventMetadata struct {
	EventID       string
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
}

type eventEnvelope struct {
	EventID       string `json:"eventId"`
	EventType     string `json:"eventType"`
	AggregateID   string `json:"aggregateId"`
	CorrelationID string `json:"correlationId"`
	CausationID   string `json:"causationId,omitempty"`
	OccurredAt    string `json:"occurredAt"`
	Version       int    `json:"version"`
}

// WagerTransactionProcessed is the typed integration event for a successful transaction.
type WagerTransactionProcessed struct {
	eventEnvelope
	Data WagerTransactionProcessedData `json:"data"`
}

// WagerTransactionProcessedData is a transaction snapshot. External fields are absent for OPENING.
type WagerTransactionProcessedData struct {
	TransactionID                  string            `json:"transactionId"`
	WalletID                       string            `json:"walletId"`
	PlayerID                       string            `json:"playerId"`
	Kind                           WagerKind         `json:"kind"`
	Origin                         TransactionOrigin `json:"origin"`
	ExternalTransactionID          string            `json:"externalTransactionId,omitempty"`
	ProviderID                     string            `json:"providerId,omitempty"`
	IdempotencyKey                 string            `json:"idempotencyKey,omitempty"`
	PayloadHash                    string            `json:"payloadHash,omitempty"`
	RoundID                        string            `json:"roundId,omitempty"`
	GameID                         string            `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string            `json:"referenceExternalTransactionId,omitempty"`
	Money                          money.Money       `json:"money"`
	Status                         TransactionStatus `json:"status"`
}

// WagerTransactionRejected is the typed integration event for a durable rejected transaction.
type WagerTransactionRejected struct {
	eventEnvelope
	Data WagerTransactionRejectedData `json:"data"`
}

type WagerTransactionRejectedData struct {
	TransactionID string            `json:"transactionId"`
	WalletID      string            `json:"walletId"`
	PlayerID      string            `json:"playerId"`
	Kind          WagerKind         `json:"kind"`
	Origin        TransactionOrigin `json:"origin"`
	FailureCode   string            `json:"failureCode"`
	Status        TransactionStatus `json:"status"`
}

// WalletBalanceChanged is the typed integration event for a ledger-backed balance movement.
type WalletBalanceChanged struct {
	eventEnvelope
	Data WalletBalanceChangedData `json:"data"`
}

type WalletBalanceChangedData struct {
	WalletID      string      `json:"walletId"`
	TransactionID string      `json:"transactionId"`
	Direction     Direction   `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	WalletVersion int64       `json:"walletVersion"`
}

// WagerTransactionPendingReference is the typed integration event for a transaction awaiting its reference.
type WagerTransactionPendingReference struct {
	eventEnvelope
	Data WagerTransactionPendingReferenceData `json:"data"`
}

type WagerTransactionPendingReferenceData struct {
	TransactionID                  string `json:"transactionId"`
	WalletID                       string `json:"walletId"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
	PendingExpiresAt               string `json:"pendingExpiresAt"`
}

// NewWagerTransactionProcessed constructs the fixed-version processed event from a processed snapshot.
func NewWagerTransactionProcessed(metadata EventMetadata, transaction *WagerTransaction) (WagerTransactionProcessed, error) {
	if transaction == nil || transaction.Status() != Processed {
		return WagerTransactionProcessed{}, eventError()
	}
	envelope, err := newEventEnvelope(metadata, WagerTransactionProcessedEventType, transaction.ID())
	if err != nil {
		return WagerTransactionProcessed{}, err
	}
	return WagerTransactionProcessed{eventEnvelope: envelope, Data: WagerTransactionProcessedData{
		TransactionID: transaction.ID(), WalletID: transaction.WalletID(), PlayerID: transaction.PlayerID(), Kind: transaction.Kind(), Origin: transaction.Origin(),
		ExternalTransactionID: transaction.ExternalTransactionID(), ProviderID: transaction.ProviderID(), IdempotencyKey: transaction.IdempotencyKey(), PayloadHash: transaction.PayloadHash(),
		RoundID: transaction.RoundID(), GameID: transaction.GameID(), ReferenceExternalTransactionID: transaction.ReferenceExternalTransactionID(), Money: transaction.Money(), Status: transaction.Status(),
	}}, nil
}

// NewWagerTransactionRejected constructs the fixed-version rejection event from a rejected snapshot.
func NewWagerTransactionRejected(metadata EventMetadata, transaction *WagerTransaction) (WagerTransactionRejected, error) {
	if transaction == nil || transaction.Status() != Rejected || transaction.FailureCode() == "" {
		return WagerTransactionRejected{}, eventError()
	}
	envelope, err := newEventEnvelope(metadata, WagerTransactionRejectedEventType, transaction.ID())
	if err != nil {
		return WagerTransactionRejected{}, err
	}
	return WagerTransactionRejected{eventEnvelope: envelope, Data: WagerTransactionRejectedData{TransactionID: transaction.ID(), WalletID: transaction.WalletID(), PlayerID: transaction.PlayerID(), Kind: transaction.Kind(), Origin: transaction.Origin(), FailureCode: transaction.FailureCode(), Status: transaction.Status()}}, nil
}

// NewWalletBalanceChanged constructs the fixed-version balance event from a movement and resulting wallet state.
func NewWalletBalanceChanged(metadata EventMetadata, walletValue *Wallet, entry *WalletLedgerEntry) (WalletBalanceChanged, error) {
	if walletValue == nil || entry == nil || walletValue.ID() != entry.WalletID() || walletValue.Version() < 1 {
		return WalletBalanceChanged{}, eventError()
	}
	envelope, err := newEventEnvelope(metadata, WalletBalanceChangedEventType, walletValue.ID())
	if err != nil {
		return WalletBalanceChanged{}, err
	}
	return WalletBalanceChanged{eventEnvelope: envelope, Data: WalletBalanceChangedData{WalletID: walletValue.ID(), TransactionID: entry.TransactionID(), Direction: entry.Direction(), Money: entry.Money(), BalanceBefore: entry.BalanceBefore(), BalanceAfter: entry.BalanceAfter(), WalletVersion: walletValue.Version()}}, nil
}

// NewWagerTransactionPendingReference constructs the fixed-version pending-reference event.
func NewWagerTransactionPendingReference(metadata EventMetadata, transaction *WagerTransaction, pendingExpiresAt time.Time) (WagerTransactionPendingReference, error) {
	if transaction == nil || transaction.Status() != PendingReference || transaction.ReferenceExternalTransactionID() == "" || pendingExpiresAt.IsZero() {
		return WagerTransactionPendingReference{}, eventError()
	}
	envelope, err := newEventEnvelope(metadata, WagerTransactionPendingReferenceEventType, transaction.ID())
	if err != nil {
		return WagerTransactionPendingReference{}, err
	}
	return WagerTransactionPendingReference{eventEnvelope: envelope, Data: WagerTransactionPendingReferenceData{TransactionID: transaction.ID(), WalletID: transaction.WalletID(), ReferenceExternalTransactionID: transaction.ReferenceExternalTransactionID(), PendingExpiresAt: pendingExpiresAt.UTC().Format(time.RFC3339)}}, nil
}

func newEventEnvelope(metadata EventMetadata, eventType, aggregateID string) (eventEnvelope, error) {
	if metadata.EventID == "" || metadata.CorrelationID == "" || metadata.OccurredAt.IsZero() || eventType == "" || aggregateID == "" {
		return eventEnvelope{}, eventError()
	}
	return eventEnvelope{EventID: metadata.EventID, EventType: eventType, AggregateID: aggregateID, CorrelationID: metadata.CorrelationID, CausationID: metadata.CausationID, OccurredAt: metadata.OccurredAt.UTC().Format(time.RFC3339), Version: eventVersion}, nil
}

func eventError() error { return &Error{Kind: ErrInvalidEvent} }
