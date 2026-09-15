package wallet_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

func TestProcessedEventHasFixedEnvelopeAndOmitsInternalMetadata(t *testing.T) {
	t.Parallel()
	transaction, err := wallet.NewOpeningTransaction(wallet.OpeningTransactionInput{ID: "transaction-1", WalletID: "wallet-1", PlayerID: "player-1", Money: mustMoney(t, "10.00", money.BRL), CreatedAt: occurredAt})
	if err != nil {
		t.Fatalf("NewOpeningTransaction() error = %v", err)
	}
	if err := transaction.MarkProcessed(occurredAt.Add(time.Minute)); err != nil {
		t.Fatalf("MarkProcessed() error = %v", err)
	}

	event, err := wallet.NewWagerTransactionProcessed(wallet.EventMetadata{EventID: "event-1", CorrelationID: "correlation-1", OccurredAt: occurredAt}, transaction)
	if err != nil {
		t.Fatalf("NewWagerTransactionProcessed() error = %v", err)
	}
	if event.EventType != wallet.WagerTransactionProcessedEventType || event.Version != 1 {
		t.Errorf("event type/version = %s/%d, want WagerTransactionProcessed/1", event.EventType, event.Version)
	}
	if event.OccurredAt != "2026-09-14T12:00:00Z" {
		t.Errorf("OccurredAt = %q, want UTC RFC 3339", event.OccurredAt)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(encoded) != `{"eventId":"event-1","eventType":"WagerTransactionProcessed","aggregateId":"transaction-1","correlationId":"correlation-1","occurredAt":"2026-09-14T12:00:00Z","version":1,"data":{"transactionId":"transaction-1","walletId":"wallet-1","playerId":"player-1","kind":"OPENING","origin":"INTERNAL","money":{"amount":"10.00","currency":"BRL"},"status":"PROCESSED"}}` {
		t.Errorf("event JSON = %s, want fixed envelope with no external metadata", encoded)
	}
}

func TestBalanceChangedEventSerializesMoneyAsDecimalStrings(t *testing.T) {
	t.Parallel()
	walletValue, _, err := wallet.Open(openInput(t, "100.00"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	entry, err := walletValue.Debit(wallet.MovementInput{LedgerEntryID: "entry-2", TransactionID: "transaction-2", Money: mustMoney(t, "80.00", money.BRL), OccurredAt: occurredAt.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Debit() error = %v", err)
	}
	event, err := wallet.NewWalletBalanceChanged(wallet.EventMetadata{EventID: "event-1", CorrelationID: "correlation-1", OccurredAt: occurredAt}, walletValue, entry)
	if err != nil {
		t.Fatalf("NewWalletBalanceChanged() error = %v", err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(encoded) != `{"eventId":"event-1","eventType":"WalletBalanceChanged","aggregateId":"wallet-1","correlationId":"correlation-1","occurredAt":"2026-09-14T12:00:00Z","version":1,"data":{"walletId":"wallet-1","transactionId":"transaction-2","direction":"DEBIT","money":{"amount":"80.00","currency":"BRL"},"balanceBefore":{"amount":"100.00","currency":"BRL"},"balanceAfter":{"amount":"20.00","currency":"BRL"},"walletVersion":2}}` {
		t.Errorf("event JSON = %s, want decimal money strings", encoded)
	}
}

func TestBalanceChangedEventAcceptsOpeningCreditAtVersionOne(t *testing.T) {
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

	event, err := wallet.NewWalletBalanceChanged(wallet.EventMetadata{EventID: "event-1", CorrelationID: "correlation-1", OccurredAt: occurredAt}, walletValue, entry)
	if err != nil {
		t.Fatalf("NewWalletBalanceChanged() error = %v", err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(encoded) != `{"eventId":"event-1","eventType":"WalletBalanceChanged","aggregateId":"wallet-1","correlationId":"correlation-1","occurredAt":"2026-09-14T12:00:00Z","version":1,"data":{"walletId":"wallet-1","transactionId":"transaction-1","direction":"CREDIT","money":{"amount":"100.00","currency":"BRL"},"balanceBefore":{"amount":"0.00","currency":"BRL"},"balanceAfter":{"amount":"100.00","currency":"BRL"},"walletVersion":1}}` {
		t.Errorf("event JSON = %s, want opening balance event at version 1", encoded)
	}
}

func TestRejectedAndPendingReferenceEventsRequireMatchingTransactionState(t *testing.T) {
	t.Parallel()
	transaction, err := wallet.NewExternalTransaction(externalTransactionInput(t, wallet.Refund, "reference-1"))
	if err != nil {
		t.Fatalf("NewExternalTransaction() error = %v", err)
	}
	if _, err := wallet.NewWagerTransactionRejected(wallet.EventMetadata{EventID: "event-1", CorrelationID: "correlation-1", OccurredAt: occurredAt}, transaction); err == nil {
		t.Error("NewWagerTransactionRejected() error = nil, want state error")
	}
	if err := transaction.MarkPendingReference(occurredAt.Add(time.Minute)); err != nil {
		t.Fatalf("MarkPendingReference() error = %v", err)
	}
	if _, err := wallet.NewWagerTransactionPendingReference(wallet.EventMetadata{EventID: "event-2", CorrelationID: "correlation-1", OccurredAt: occurredAt}, transaction, occurredAt.Add(time.Hour)); err != nil {
		t.Fatalf("NewWagerTransactionPendingReference() error = %v", err)
	}
	if err := transaction.MarkRejected("REFERENCE_NOT_FOUND", occurredAt.Add(2*time.Minute)); err != nil {
		t.Fatalf("MarkRejected() error = %v", err)
	}
	if _, err := wallet.NewWagerTransactionRejected(wallet.EventMetadata{EventID: "event-3", CorrelationID: "correlation-1", OccurredAt: occurredAt}, transaction); err != nil {
		t.Fatalf("NewWagerTransactionRejected() error = %v", err)
	}
}
