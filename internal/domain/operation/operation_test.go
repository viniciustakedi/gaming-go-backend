package operation_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

func TestPayloadHashMatchesFixedCanonicalVectorAndOmitsAbsentReference(t *testing.T) {
	t.Parallel()

	amount, err := money.Parse("25.00", money.BRL)
	if err != nil {
		t.Fatalf("money.Parse() error = %v", err)
	}

	hash, err := operation.PayloadHash(operation.Request{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123",
		PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37",
		RoundID: "round-987", GameID: "rock & roll <x> ação", Kind: wallet.Bet, Money: amount,
	})
	if err != nil {
		t.Fatalf("PayloadHash() error = %v", err)
	}

	// Calculated independently with:
	// printf '%s' '{"externalTransactionId":"transaction-123","gameId":"rock & roll <x> ação","kind":"BET","money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}' | shasum -a 256
	const want = "40d77b698222255101313a959c457c08e6ad02e88082cdf96a6c31df7985d600"
	if hash != want {
		t.Errorf("PayloadHash() = %q, want fixed SHA-256 vector %q", hash, want)
	}
}

func TestPayloadHashRejectsNonCanonicalUUIDs(t *testing.T) {
	t.Parallel()

	_, err := operation.PayloadHash(operation.Request{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123", PlayerID: "0192F28F-5DC0-7D58-BDB2-814AD6A0F4A1",
		WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37", RoundID: "round-987", GameID: "fortune-chimp", Kind: wallet.Bet, Money: mustMoney(t, "25.00"),
	})
	if !errors.Is(err, operation.ErrInvalidRequest) {
		t.Errorf("PayloadHash() error = %v, want errors.Is(_, INVALID_REQUEST)", err)
	}
}

func TestPayloadHashRejectsInvalidIdentityAndEmptyReference(t *testing.T) {
	t.Parallel()

	emptyReference := ""
	tests := []struct {
		name   string
		mutate func(*operation.Request)
	}{
		{name: "empty provider", mutate: func(request *operation.Request) { request.ProviderID = "" }},
		{name: "empty external transaction", mutate: func(request *operation.Request) { request.ExternalTransactionID = "" }},
		{name: "empty round", mutate: func(request *operation.Request) { request.RoundID = "" }},
		{name: "empty game", mutate: func(request *operation.Request) { request.GameID = "" }},
		{name: "oversized provider", mutate: func(request *operation.Request) { request.ProviderID = opaqueIdentifier(256) }},
		{name: "oversized external transaction", mutate: func(request *operation.Request) { request.ExternalTransactionID = opaqueIdentifier(256) }},
		{name: "oversized round", mutate: func(request *operation.Request) { request.RoundID = opaqueIdentifier(256) }},
		{name: "oversized game", mutate: func(request *operation.Request) { request.GameID = opaqueIdentifier(256) }},
		{name: "empty reference", mutate: func(request *operation.Request) { request.ReferenceExternalTransactionID = &emptyReference }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := validRequest(t, wallet.Bet, "25.00")
			tt.mutate(&request)
			if _, err := operation.PayloadHash(request); !errors.Is(err, operation.ErrInvalidRequest) {
				t.Errorf("PayloadHash() error = %v, want INVALID_REQUEST", err)
			}
		})
	}
}

func TestPayloadHashMatchesForEquivalentHTTPAndSQSData(t *testing.T) {
	t.Parallel()

	const httpBody = `{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}`
	const sqsEnvelope = `{"messageId":"message-123","type":"WagerRequested","occurredAt":"2026-09-14T12:00:00Z","data":{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`
	var httpRequest operation.Request
	if err := json.Unmarshal([]byte(httpBody), &httpRequest); err != nil {
		t.Fatalf("json.Unmarshal(HTTP body) error = %v", err)
	}
	var envelope struct {
		Data operation.Request `json:"data"`
	}
	if err := json.Unmarshal([]byte(sqsEnvelope), &envelope); err != nil {
		t.Fatalf("json.Unmarshal(SQS envelope) error = %v", err)
	}
	httpHash, err := operation.PayloadHash(httpRequest)
	if err != nil {
		t.Fatalf("PayloadHash(HTTP body) error = %v", err)
	}
	sqsHash, err := operation.PayloadHash(envelope.Data)
	if err != nil {
		t.Fatalf("PayloadHash(SQS data) error = %v", err)
	}
	if httpHash != sqsHash {
		t.Errorf("equivalent HTTP and SQS hashes = %q and %q, want equality", httpHash, sqsHash)
	}
}

func TestCatalogClassifiesEveryStableCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code operation.Code
		want operation.Classification
	}{
		{operation.CodeInvalidRequest, operation.Correctable}, {operation.CodeInvalidMoney, operation.Correctable}, {operation.CodeUnsupportedCurrency, operation.Correctable}, {operation.CodeInvalidAmountForKind, operation.Correctable}, {operation.CodeKindNotAllowed, operation.Correctable}, {operation.CodeMissingIdempotencyKey, operation.Correctable}, {operation.CodeReferenceRequired, operation.Correctable}, {operation.CodeReferenceNotAllowed, operation.Correctable}, {operation.CodeWalletNotFound, operation.Correctable}, {operation.CodeWalletPlayerMismatch, operation.Correctable}, {operation.CodeWalletCurrencyMismatch, operation.Correctable},
		{operation.CodeInsufficientFunds, operation.Definitive}, {operation.CodeReversalInsufficientFunds, operation.Definitive}, {operation.CodeReferenceNotFound, operation.Definitive}, {operation.CodeReferenceNotProcessed, operation.Definitive}, {operation.CodeReferenceAlreadyReversed, operation.Definitive}, {operation.CodeReferenceKindNotReversible, operation.Definitive}, {operation.CodeReferenceMismatch, operation.Definitive}, {operation.CodeReferenceAmountMismatch, operation.Definitive},
		{operation.CodePermanentProcessingFailure, operation.PermanentFailure}, {operation.CodeIdempotencyKeyReused, operation.Conflict}, {operation.CodeExternalTransactionIDConflict, operation.Conflict}, {operation.CodeWalletAlreadyExists, operation.Conflict}, {operation.CodeTemporarilyUnavailable, operation.Unavailable},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			got, ok := operation.ClassificationFor(tt.code)
			if !ok || got != tt.want {
				t.Errorf("ClassificationFor(%s) = (%s, %t), want (%s, true)", tt.code, got, ok, tt.want)
			}
		})
	}
}

func TestEvaluateBetMovesAsDebit(t *testing.T) {
	t.Parallel()

	decision, err := operation.Evaluate(validRequest(t, wallet.Bet, "25.00"), nil, false)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if decision.Action != operation.Process || decision.Direction != wallet.Debit {
		t.Errorf("Evaluate() = %+v, want a debit processing decision", decision)
	}
}

func TestEvaluateRollbackOfWinMovesAsDebit(t *testing.T) {
	t.Parallel()

	referenceID := "bet-1"
	request := validRequest(t, wallet.Rollback, "25.00")
	request.ReferenceExternalTransactionID = &referenceID
	decision, err := operation.Evaluate(request, processedReference(t, wallet.Win, "25.00"), false)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if decision.Action != operation.Process || decision.Direction != wallet.Debit {
		t.Errorf("Evaluate() = %+v, want a debit processing decision", decision)
	}
}

func TestClassifyAttemptDistinguishesReplayAndConflicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		existingByKey      *operation.Attempt
		existingByExternal *operation.Attempt
		want               operation.AttemptKind
		wantError          error
	}{
		{name: "new", want: operation.NewAttempt},
		{name: "replay", existingByKey: &operation.Attempt{IdempotencyKey: "key-1", PayloadHash: "hash-1", ExternalTransactionID: "external-1"}, want: operation.Replay},
		{name: "reused key", existingByKey: &operation.Attempt{IdempotencyKey: "key-1", PayloadHash: "different", ExternalTransactionID: "external-1"}, want: operation.AttemptConflict, wantError: operation.ErrIdempotencyKeyReused},
		{name: "external transaction conflict", existingByExternal: &operation.Attempt{IdempotencyKey: "other-key", PayloadHash: "hash-1", ExternalTransactionID: "external-1"}, want: operation.AttemptConflict, wantError: operation.ErrExternalTransactionIDConflict},
		{name: "reused key wins double collision", existingByKey: &operation.Attempt{IdempotencyKey: "key-1", PayloadHash: "different", ExternalTransactionID: "another-external"}, existingByExternal: &operation.Attempt{IdempotencyKey: "other-key", PayloadHash: "hash-1", ExternalTransactionID: "external-1"}, want: operation.AttemptConflict, wantError: operation.ErrIdempotencyKeyReused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := operation.ClassifyAttempt(operation.Attempt{IdempotencyKey: "key-1", PayloadHash: "hash-1", ExternalTransactionID: "external-1"}, tt.existingByKey, tt.existingByExternal)
			if decision.Kind != tt.want || (tt.wantError == nil && decision.Error != nil) || (tt.wantError != nil && !errors.Is(decision.Error, tt.wantError)) {
				t.Errorf("ClassifyAttempt() = %+v, want kind %s and error %v", decision, tt.want, tt.wantError)
			}
		})
	}
}

func TestEvaluateOperationAndReferenceMatrix(t *testing.T) {
	t.Parallel()
	referenceID := "reference-external-id"
	tests := []struct {
		name          string
		request       operation.Request
		reference     *wallet.WagerTransaction
		alreadyRevert bool
		wantAction    operation.Action
		wantDirection wallet.Direction
		wantError     error
	}{
		{name: "win credits without reference", request: operation.Request{Kind: wallet.Win, Money: mustMoney(t, "25.00")}, wantAction: operation.Process, wantDirection: wallet.Credit},
		{name: "loss does not move money", request: operation.Request{Kind: wallet.Loss, Money: mustMoney(t, "0.00")}, wantAction: operation.Process},
		{name: "refund credits processed bet", request: operation.Request{Kind: wallet.Refund, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: processedReference(t, wallet.Bet, "25.00"), wantAction: operation.Process, wantDirection: wallet.Credit},
		{name: "rollback credits processed bet", request: operation.Request{Kind: wallet.Rollback, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: processedReference(t, wallet.Bet, "25.00"), wantAction: operation.Process, wantDirection: wallet.Credit},
		{name: "rollback debits processed refund", request: operation.Request{Kind: wallet.Rollback, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: processedReference(t, wallet.Refund, "25.00"), wantAction: operation.Process, wantDirection: wallet.Debit},
		{name: "missing reversal reference waits", request: operation.Request{Kind: wallet.Refund, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, wantAction: operation.WaitForReference},
		{name: "pending reference waits", request: operation.Request{Kind: wallet.Refund, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: pendingReference(t), wantAction: operation.WaitForReference},
		{name: "missing win reference waits", request: operation.Request{Kind: wallet.Win, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, wantAction: operation.WaitForReference},
		{name: "pending win reference waits", request: operation.Request{Kind: wallet.Win, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: pendingReference(t), wantAction: operation.WaitForReference},
		{name: "missing rollback reference waits", request: operation.Request{Kind: wallet.Rollback, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, wantAction: operation.WaitForReference},
		{name: "pending rollback reference waits", request: operation.Request{Kind: wallet.Rollback, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: pendingReference(t), wantAction: operation.WaitForReference},
		{name: "pending transaction reference is terminal rejection", request: operation.Request{Kind: wallet.Refund, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: pendingTransaction(t), wantAction: operation.Reject, wantError: operation.ErrReferenceNotProcessed},
		{name: "rejected reference is terminal rejection", request: operation.Request{Kind: wallet.Refund, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: mustRejectedReference(t), wantAction: operation.Reject, wantError: operation.ErrReferenceNotProcessed},
		{name: "rollback of loss is rejected", request: operation.Request{Kind: wallet.Rollback, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: processedReference(t, wallet.Loss, "25.00"), wantAction: operation.Reject, wantError: operation.ErrReferenceKindNotReversible},
		{name: "rollback of rollback is rejected", request: operation.Request{Kind: wallet.Rollback, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: processedReference(t, wallet.Rollback, "25.00"), wantAction: operation.Reject, wantError: operation.ErrReferenceKindNotReversible},
		{name: "reversal amount must match", request: operation.Request{Kind: wallet.Refund, Money: mustMoney(t, "20.00"), ReferenceExternalTransactionID: &referenceID}, reference: processedReference(t, wallet.Bet, "25.00"), wantAction: operation.Reject, wantError: operation.ErrReferenceAmountMismatch},
		{name: "reference identity must match", request: operation.Request{ProviderID: "provider-a", PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a2", WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37", RoundID: "round-id", Kind: wallet.Refund, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: processedReference(t, wallet.Bet, "25.00"), wantAction: operation.Reject, wantError: operation.ErrReferenceMismatch},
		{name: "win reference must be a bet", request: operation.Request{Kind: wallet.Win, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: processedReference(t, wallet.Win, "25.00"), wantAction: operation.Reject, wantError: operation.ErrReferenceKindNotReversible},
		{name: "second reversal is rejected", request: operation.Request{Kind: wallet.Refund, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, reference: processedReference(t, wallet.Bet, "25.00"), alreadyRevert: true, wantAction: operation.Reject, wantError: operation.ErrReferenceAlreadyReversed},
		{name: "opening is not an external operation", request: operation.Request{Kind: wallet.Opening, Money: mustMoney(t, "25.00")}, wantError: operation.ErrKindNotAllowed},
		{name: "zero bet is invalid", request: operation.Request{Kind: wallet.Bet, Money: mustMoney(t, "0.00")}, wantError: operation.ErrInvalidAmountForKind},
		{name: "non-zero loss is invalid", request: operation.Request{Kind: wallet.Loss, Money: mustMoney(t, "25.00")}, wantError: operation.ErrInvalidAmountForKind},
		{name: "bet reference is not allowed", request: operation.Request{Kind: wallet.Bet, Money: mustMoney(t, "25.00"), ReferenceExternalTransactionID: &referenceID}, wantError: operation.ErrReferenceNotAllowed},
		{name: "refund requires reference", request: operation.Request{Kind: wallet.Refund, Money: mustMoney(t, "25.00")}, wantError: operation.ErrReferenceRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.request = withValidIdentity(t, tt.request)
			decision, err := operation.Evaluate(tt.request, tt.reference, tt.alreadyRevert)
			if tt.wantError != nil && tt.wantAction == "" {
				if !errors.Is(err, tt.wantError) {
					t.Errorf("Evaluate() error = %v, want errors.Is(_, %v)", err, tt.wantError)
				}
				return
			}
			errorMatches := (tt.wantError == nil && decision.Error == nil) || (tt.wantError != nil && errors.Is(decision.Error, tt.wantError))
			if err != nil || decision.Action != tt.wantAction || decision.Direction != tt.wantDirection || !errorMatches {
				t.Errorf("Evaluate() = (%+v, %v), want action %s, direction %s, error %v", decision, err, tt.wantAction, tt.wantDirection, tt.wantError)
			}
		})
	}
}

func TestEvaluateRejectsInvalidIdentityBeforeReferenceRules(t *testing.T) {
	t.Parallel()

	referenceID := "reference-external-id"
	tests := []struct {
		name   string
		mutate func(*operation.Request)
	}{
		{name: "player", mutate: func(request *operation.Request) { request.PlayerID = "" }},
		{name: "wallet", mutate: func(request *operation.Request) { request.WalletID = "" }},
		{name: "round", mutate: func(request *operation.Request) { request.RoundID = "" }},
		{name: "provider", mutate: func(request *operation.Request) { request.ProviderID = "" }},
		{name: "external transaction", mutate: func(request *operation.Request) { request.ExternalTransactionID = "" }},
		{name: "game", mutate: func(request *operation.Request) { request.GameID = "" }},
		{name: "non-canonical player UUID", mutate: func(request *operation.Request) { request.PlayerID = "0192F28F-5DC0-7D58-BDB2-814AD6A0F4A1" }},
		{name: "oversized round", mutate: func(request *operation.Request) { request.RoundID = opaqueIdentifier(256) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := validRequest(t, wallet.Refund, "25.00")
			request.ReferenceExternalTransactionID = &referenceID
			tt.mutate(&request)
			if _, err := operation.Evaluate(request, processedReference(t, wallet.Bet, "25.00"), false); !errors.Is(err, operation.ErrInvalidRequest) {
				t.Errorf("Evaluate() error = %v, want INVALID_REQUEST", err)
			}
		})
	}
}

func TestEvaluateRejectsEachReferenceIdentityMismatch(t *testing.T) {
	t.Parallel()

	referenceID := "reference-external-id"
	tests := []struct {
		name   string
		mutate func(*operation.Request)
	}{
		{name: "player", mutate: func(request *operation.Request) { request.PlayerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a2" }},
		{name: "wallet", mutate: func(request *operation.Request) { request.WalletID = "0192f291-27dd-7d3f-8071-5f8685deef38" }},
		{name: "round", mutate: func(request *operation.Request) { request.RoundID = "another-round" }},
		{name: "currency", mutate: func(request *operation.Request) { request.Money = mustMoneyIn(t, "25.00", money.USD) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := validRequest(t, wallet.Refund, "25.00")
			request.ReferenceExternalTransactionID = &referenceID
			tt.mutate(&request)
			decision, err := operation.Evaluate(request, processedReference(t, wallet.Bet, "25.00"), false)
			if err != nil || decision.Action != operation.Reject || !errors.Is(decision.Error, operation.ErrReferenceMismatch) {
				t.Errorf("Evaluate() = (%+v, %v), want REFERENCE_MISMATCH rejection", decision, err)
			}
		})
	}
}

func TestPendingReferenceExpiryAndInsufficientFundsClassification(t *testing.T) {
	t.Parallel()

	referenceID := "reference-external-id"
	request := validRequest(t, wallet.Refund, "25.00")
	request.ReferenceExternalTransactionID = &referenceID
	tests := []struct {
		name      string
		reference *wallet.WagerTransaction
		want      *operation.Error
	}{
		{name: "absent", want: operation.ErrReferenceNotFound},
		{name: "pending reference", reference: pendingReference(t), want: operation.ErrReferenceNotProcessed},
		{name: "pending", reference: pendingTransaction(t), want: operation.ErrReferenceNotProcessed},
		{name: "rejected", reference: mustRejectedReference(t), want: operation.ErrReferenceNotProcessed},
		{name: "failed", reference: failedReference(t), want: operation.ErrReferenceNotProcessed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := operation.ExpirePendingReference(request, tt.reference, false)
			if err != nil || decision.Action != operation.Reject || !errors.Is(decision.Error, tt.want) {
				t.Errorf("ExpirePendingReference() = (%+v, %v), want %v rejection", decision, err, tt.want)
			}
		})
	}
	processed := processedReference(t, wallet.Bet, "25.00")
	want, wantErr := operation.Evaluate(request, processed, false)
	got, gotErr := operation.ExpirePendingReference(request, processed, false)
	if got != want || !errors.Is(gotErr, wantErr) {
		t.Errorf("ExpirePendingReference(PROCESSED) = (%+v, %v), want Evaluate() = (%+v, %v)", got, gotErr, want, wantErr)
	}
	if got := operation.InsufficientFundsFor(wallet.Bet); !errors.Is(got, operation.ErrInsufficientFunds) {
		t.Errorf("InsufficientFundsFor(BET) = %v, want INSUFFICIENT_FUNDS", got)
	}
	if got := operation.InsufficientFundsFor(wallet.Rollback); !errors.Is(got, operation.ErrReversalInsufficientFunds) {
		t.Errorf("InsufficientFundsFor(ROLLBACK) = %v, want REVERSAL_INSUFFICIENT_FUNDS", got)
	}
}

func mustMoney(t *testing.T, amount string) money.Money {
	return mustMoneyIn(t, amount, money.BRL)
}

func mustMoneyIn(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	value, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("money.Parse(%q) error = %v", amount, err)
	}
	return value
}

func validRequest(t *testing.T, kind wallet.WagerKind, amount string) operation.Request {
	t.Helper()
	return operation.Request{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123",
		PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37",
		RoundID: "round-id", GameID: "game-id", Kind: kind, Money: mustMoney(t, amount),
	}
}

func withValidIdentity(t *testing.T, request operation.Request) operation.Request {
	t.Helper()
	base := validRequest(t, request.Kind, "25.00")
	if request.ProviderID == "" {
		request.ProviderID = base.ProviderID
	}
	if request.ExternalTransactionID == "" {
		request.ExternalTransactionID = base.ExternalTransactionID
	}
	if request.PlayerID == "" {
		request.PlayerID = base.PlayerID
	}
	if request.WalletID == "" {
		request.WalletID = base.WalletID
	}
	if request.RoundID == "" {
		request.RoundID = base.RoundID
	}
	if request.GameID == "" {
		request.GameID = base.GameID
	}
	return request
}

func opaqueIdentifier(length int) string {
	return string(make([]byte, length))
}

func processedReference(t *testing.T, kind wallet.WagerKind, amount string) *wallet.WagerTransaction {
	t.Helper()
	input := wallet.ExternalTransactionInput{
		ID: "reference-id", ExternalTransactionID: "reference-external-id", ProviderID: "provider-a", IdempotencyKey: "reference-key", PayloadHash: "reference-hash",
		WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37", PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", RoundID: "round-id", GameID: "game-id", Kind: kind, Money: mustMoney(t, amount), CreatedAt: time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC),
	}
	if kind == wallet.Refund || kind == wallet.Rollback {
		input.ReferenceExternalTransactionID = "another-reference"
	}
	reference, err := wallet.NewExternalTransaction(input)
	if err != nil {
		t.Fatalf("NewExternalTransaction() error = %v", err)
	}
	if err := reference.MarkProcessed(time.Date(2026, time.September, 14, 12, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MarkProcessed() error = %v", err)
	}
	return reference
}

func pendingReference(t *testing.T) *wallet.WagerTransaction {
	t.Helper()
	reference, err := wallet.NewExternalTransaction(wallet.ExternalTransactionInput{
		ID: "reference-id", ExternalTransactionID: "reference-external-id", ProviderID: "provider-a", IdempotencyKey: "reference-key", PayloadHash: "reference-hash",
		WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37", PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", RoundID: "round-id", GameID: "game-id", Kind: wallet.Bet, Money: mustMoney(t, "25.00"), CreatedAt: time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewExternalTransaction() error = %v", err)
	}
	if err := reference.MarkPendingReference(time.Date(2026, time.September, 14, 12, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MarkPendingReference() error = %v", err)
	}
	return reference
}

func pendingTransaction(t *testing.T) *wallet.WagerTransaction {
	t.Helper()
	reference, err := wallet.NewExternalTransaction(wallet.ExternalTransactionInput{
		ID: "reference-id", ExternalTransactionID: "reference-external-id", ProviderID: "provider-a", IdempotencyKey: "reference-key", PayloadHash: "reference-hash",
		WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37", PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", RoundID: "round-id", GameID: "game-id", Kind: wallet.Bet, Money: mustMoney(t, "25.00"), CreatedAt: time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewExternalTransaction() error = %v", err)
	}
	return reference
}

func mustRejectedReference(t *testing.T) *wallet.WagerTransaction {
	t.Helper()
	reference, err := wallet.NewExternalTransaction(wallet.ExternalTransactionInput{
		ID: "reference-id", ExternalTransactionID: "reference-external-id", ProviderID: "provider-a", IdempotencyKey: "reference-key", PayloadHash: "reference-hash",
		WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37", PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", RoundID: "round-id", GameID: "game-id", Kind: wallet.Bet, Money: mustMoney(t, "25.00"), CreatedAt: time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewExternalTransaction() error = %v", err)
	}
	if err := reference.MarkRejected("INSUFFICIENT_FUNDS", time.Date(2026, time.September, 14, 12, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MarkRejected() error = %v", err)
	}
	return reference
}

func failedReference(t *testing.T) *wallet.WagerTransaction {
	t.Helper()
	reference, err := wallet.NewExternalTransaction(wallet.ExternalTransactionInput{
		ID: "reference-id", ExternalTransactionID: "reference-external-id", ProviderID: "provider-a", IdempotencyKey: "reference-key", PayloadHash: "reference-hash",
		WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37", PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", RoundID: "round-id", GameID: "game-id", Kind: wallet.Bet, Money: mustMoney(t, "25.00"), CreatedAt: time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewExternalTransaction() error = %v", err)
	}
	if err := reference.MarkFailed("PERMANENT_PROCESSING_FAILURE", time.Date(2026, time.September, 14, 12, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MarkFailed() error = %v", err)
	}
	return reference
}
