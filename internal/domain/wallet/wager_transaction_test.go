package wallet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

func TestTerminalTransactionRejectsFurtherTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		enter func(*wallet.WagerTransaction) error
	}{
		{name: "processed", enter: func(transaction *wallet.WagerTransaction) error {
			return transaction.MarkProcessed(occurredAt.Add(time.Minute))
		}},
		{name: "rejected", enter: func(transaction *wallet.WagerTransaction) error {
			return transaction.MarkRejected("INSUFFICIENT_FUNDS", occurredAt.Add(time.Minute))
		}},
		{name: "failed", enter: func(transaction *wallet.WagerTransaction) error {
			return transaction.MarkFailed("PERMANENT_PROCESSING_FAILURE", occurredAt.Add(time.Minute))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transaction, err := wallet.NewExternalTransaction(externalTransactionInput(t, wallet.Bet, ""))
			if err != nil {
				t.Fatalf("NewExternalTransaction() error = %v", err)
			}
			if err := tt.enter(transaction); err != nil {
				t.Fatalf("enter terminal state error = %v", err)
			}
			if err := transaction.MarkProcessed(occurredAt.Add(2 * time.Minute)); !errors.Is(err, wallet.ErrInvalidTransition) {
				t.Errorf("terminal transition error = %v, want errors.Is(_, ErrInvalidTransition)", err)
			}
		})
	}
}

func TestPendingTransactionCanReachEveryAllowedTerminalState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		apply func(*wallet.WagerTransaction) error
		want  wallet.TransactionStatus
	}{
		{name: "processed", apply: func(transaction *wallet.WagerTransaction) error {
			return transaction.MarkProcessed(occurredAt.Add(time.Minute))
		}, want: wallet.Processed},
		{name: "rejected", apply: func(transaction *wallet.WagerTransaction) error {
			return transaction.MarkRejected("INSUFFICIENT_FUNDS", occurredAt.Add(time.Minute))
		}, want: wallet.Rejected},
		{name: "failed", apply: func(transaction *wallet.WagerTransaction) error {
			return transaction.MarkFailed("PERMANENT_PROCESSING_FAILURE", occurredAt.Add(time.Minute))
		}, want: wallet.Failed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transaction, err := wallet.NewExternalTransaction(externalTransactionInput(t, wallet.Bet, ""))
			if err != nil {
				t.Fatalf("NewExternalTransaction() error = %v", err)
			}
			if err := tt.apply(transaction); err != nil {
				t.Fatalf("transition error = %v", err)
			}
			if transaction.Status() != tt.want {
				t.Errorf("Status() = %s, want %s", transaction.Status(), tt.want)
			}
		})
	}
}

func TestPendingReferenceTransactionCanReachEveryTerminalState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		apply func(*wallet.WagerTransaction) error
		want  wallet.TransactionStatus
	}{
		{name: "processed", apply: func(transaction *wallet.WagerTransaction) error {
			return transaction.MarkProcessed(occurredAt.Add(2 * time.Minute))
		}, want: wallet.Processed},
		{name: "rejected", apply: func(transaction *wallet.WagerTransaction) error {
			return transaction.MarkRejected("REFERENCE_NOT_FOUND", occurredAt.Add(2*time.Minute))
		}, want: wallet.Rejected},
		{name: "failed", apply: func(transaction *wallet.WagerTransaction) error {
			return transaction.MarkFailed("PERMANENT_PROCESSING_FAILURE", occurredAt.Add(2*time.Minute))
		}, want: wallet.Failed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transaction, err := wallet.NewExternalTransaction(externalTransactionInput(t, wallet.Refund, "reference-1"))
			if err != nil {
				t.Fatalf("NewExternalTransaction() error = %v", err)
			}
			if err := transaction.MarkPendingReference(occurredAt.Add(time.Minute)); err != nil {
				t.Fatalf("MarkPendingReference() error = %v", err)
			}
			if err := tt.apply(transaction); err != nil {
				t.Fatalf("terminal transition error = %v", err)
			}
			if transaction.Status() != tt.want {
				t.Errorf("Status() = %s, want %s", transaction.Status(), tt.want)
			}
		})
	}
}

func TestExternalTransactionEnforcesStructuralInvariants(t *testing.T) {
	t.Parallel()
	negativeMoney, err := money.New(-1, money.BRL)
	if err != nil {
		t.Fatalf("money.New() error = %v", err)
	}
	tests := []struct {
		name  string
		input func(*testing.T) wallet.ExternalTransactionInput
	}{
		{name: "external opening", input: func(t *testing.T) wallet.ExternalTransactionInput {
			return externalTransactionInput(t, wallet.Opening, "")
		}},
		{name: "missing external transaction ID", input: func(t *testing.T) wallet.ExternalTransactionInput {
			input := externalTransactionInput(t, wallet.Bet, "")
			input.ExternalTransactionID = ""
			return input
		}},
		{name: "missing provider", input: func(t *testing.T) wallet.ExternalTransactionInput {
			input := externalTransactionInput(t, wallet.Bet, "")
			input.ProviderID = ""
			return input
		}},
		{name: "missing idempotency key", input: func(t *testing.T) wallet.ExternalTransactionInput {
			input := externalTransactionInput(t, wallet.Bet, "")
			input.IdempotencyKey = ""
			return input
		}},
		{name: "missing payload hash", input: func(t *testing.T) wallet.ExternalTransactionInput {
			input := externalTransactionInput(t, wallet.Bet, "")
			input.PayloadHash = ""
			return input
		}},
		{name: "missing round ID", input: func(t *testing.T) wallet.ExternalTransactionInput {
			input := externalTransactionInput(t, wallet.Bet, "")
			input.RoundID = ""
			return input
		}},
		{name: "missing game ID", input: func(t *testing.T) wallet.ExternalTransactionInput {
			input := externalTransactionInput(t, wallet.Bet, "")
			input.GameID = ""
			return input
		}},
		{name: "refund without reference", input: func(t *testing.T) wallet.ExternalTransactionInput {
			return externalTransactionInput(t, wallet.Refund, "")
		}},
		{name: "rollback without reference", input: func(t *testing.T) wallet.ExternalTransactionInput {
			return externalTransactionInput(t, wallet.Rollback, "")
		}},
		{name: "invalid money", input: func(t *testing.T) wallet.ExternalTransactionInput {
			input := externalTransactionInput(t, wallet.Bet, "")
			input.Money = money.Money{}
			return input
		}},
		{name: "negative money", input: func(t *testing.T) wallet.ExternalTransactionInput {
			input := externalTransactionInput(t, wallet.Bet, "")
			input.Money = negativeMoney
			return input
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := wallet.NewExternalTransaction(tt.input(t))
			if !errors.Is(err, wallet.ErrInvalidTransaction) {
				t.Errorf("NewExternalTransaction() error = %v, want errors.Is(_, ErrInvalidTransaction)", err)
			}
		})
	}
}

func TestExternalTransactionAcceptsNonNegativeMoneyAndKindPolicyInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		kind      wallet.WagerKind
		amount    string
		reference string
	}{
		{name: "zero bet", kind: wallet.Bet, amount: "0.00"},
		{name: "nonzero loss", kind: wallet.Loss, amount: "10.00"},
		{name: "bet with reference", kind: wallet.Bet, amount: "10.00", reference: "reference-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := wallet.NewExternalTransaction(externalTransactionInput(t, tt.kind, tt.reference, tt.amount)); err != nil {
				t.Fatalf("NewExternalTransaction() error = %v", err)
			}
		})
	}
}

func TestRehydrateTransactionValidatesPersistedStateWithoutTransition(t *testing.T) {
	t.Parallel()
	input := wallet.RehydratedTransaction{
		ID: "transaction-1", ExternalTransactionID: "external-1", ProviderID: "provider-1", IdempotencyKey: "key-1", PayloadHash: "hash-1",
		WalletID: "wallet-1", PlayerID: "player-1", RoundID: "round-1", GameID: "game-1", Kind: wallet.Refund, Origin: wallet.External,
		Money: mustMoney(t, "10.00", money.BRL), ReferenceExternalTransactionID: "reference-1", Status: wallet.PendingReference,
		CreatedAt: occurredAt, UpdatedAt: occurredAt.Add(time.Minute),
	}
	transaction, err := wallet.RehydrateTransaction(input)
	if err != nil {
		t.Fatalf("RehydrateTransaction() error = %v", err)
	}
	if transaction.Status() != wallet.PendingReference || transaction.CreatedAt() != occurredAt || transaction.UpdatedAt() != occurredAt.Add(time.Minute) {
		t.Errorf("rehydrated transaction = status %s, created %s, updated %s; want PENDING_REFERENCE without transition", transaction.Status(), transaction.CreatedAt(), transaction.UpdatedAt())
	}

	tests := []struct {
		name   string
		change func(*wallet.RehydratedTransaction)
	}{
		{name: "invalid status", change: func(input *wallet.RehydratedTransaction) { input.Status = "UNKNOWN" }},
		{name: "invalid money", change: func(input *wallet.RehydratedTransaction) { input.Money = money.Money{} }},
		{name: "internal non-opening", change: func(input *wallet.RehydratedTransaction) {
			input.Origin, input.Kind = wallet.Internal, wallet.Bet
			input.ExternalTransactionID, input.ProviderID, input.IdempotencyKey, input.PayloadHash, input.RoundID, input.GameID = "", "", "", "", "", ""
		}},
		{name: "refund without reference", change: func(input *wallet.RehydratedTransaction) { input.ReferenceExternalTransactionID = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invalid := input
			tt.change(&invalid)
			if _, err := wallet.RehydrateTransaction(invalid); !errors.Is(err, wallet.ErrInvalidTransaction) {
				t.Errorf("RehydrateTransaction() error = %v, want errors.Is(_, ErrInvalidTransaction)", err)
			}
		})
	}
}

func TestOpeningTransactionHasNoExternalMetadata(t *testing.T) {
	t.Parallel()
	transaction, err := wallet.NewOpeningTransaction(wallet.OpeningTransactionInput{ID: "transaction-1", WalletID: "wallet-1", PlayerID: "player-1", Money: mustMoney(t, "10.00", money.BRL), CreatedAt: occurredAt})
	if err != nil {
		t.Fatalf("NewOpeningTransaction() error = %v", err)
	}
	if transaction.Origin() != wallet.Internal || transaction.Kind() != wallet.Opening {
		t.Errorf("origin/kind = %s/%s, want INTERNAL/OPENING", transaction.Origin(), transaction.Kind())
	}
	if transaction.ProviderID() != "" || transaction.ExternalTransactionID() != "" {
		t.Error("opening transaction exposes external metadata")
	}
}

func externalTransactionInput(t *testing.T, kind wallet.WagerKind, reference string, amounts ...string) wallet.ExternalTransactionInput {
	t.Helper()
	amount := "10.00"
	if len(amounts) == 1 {
		amount = amounts[0]
	}
	return wallet.ExternalTransactionInput{ID: "transaction-1", ExternalTransactionID: "external-1", ProviderID: "provider-1", IdempotencyKey: "key-1", PayloadHash: "hash-1", WalletID: "wallet-1", PlayerID: "player-1", RoundID: "round-1", GameID: "game-1", Kind: kind, Money: mustMoney(t, amount, money.BRL), ReferenceExternalTransactionID: reference, CreatedAt: occurredAt}
}
