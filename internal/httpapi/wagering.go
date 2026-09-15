package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/auth"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

type wageringTransactionRequest struct {
	ProviderID                     string                 `json:"providerId"`
	ExternalTransactionID          string                 `json:"externalTransactionId"`
	PlayerID                       string                 `json:"playerId"`
	WalletID                       string                 `json:"walletId"`
	RoundID                        string                 `json:"roundId"`
	GameID                         string                 `json:"gameId"`
	Kind                           domainwallet.WagerKind `json:"kind"`
	Money                          money.Money            `json:"money"`
	ReferenceExternalTransactionID *string                `json:"referenceExternalTransactionId,omitempty"`
}

type wageringTransactionResponse struct {
	TransactionID    string                         `json:"transactionId"`
	Status           domainwallet.TransactionStatus `json:"status"`
	FailureCode      string                         `json:"failureCode,omitempty"`
	Balance          *money.Money                   `json:"balance,omitempty"`
	PendingExpiresAt *time.Time                     `json:"pendingExpiresAt,omitempty"`
	IdempotentReplay bool                           `json:"idempotentReplay"`
}

// wageringTransactionsHandler implements POST /wagering/transactions (spec,
// "Contratos HTTP"). The route itself requires the provider role
// (requireRole, wired in server.go); this handler additionally refuses a
// body whose providerId does not match the caller's own token claim, with
// zero financial effect - the check runs before the use case is ever called
// (spec, decision 7: "providerId do corpo diferente de provider_id devolve
// 403").
func wageringTransactionsHandler(useCase *walletapp.ProcessOperationUseCase, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFromContext(r.Context())
		if !ok {
			writeAuthError(w, http.StatusUnauthorized, codeUnauthorized, "missing or malformed bearer token")
			return
		}

		var req wageringTransactionRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			opErr, details := classifyWageringDecodeError(err)
			writeWageringError(w, opErr, details)
			return
		}

		if req.ProviderID != identity.ProviderID {
			writeAuthError(w, http.StatusForbidden, codeForbidden, "providerId does not match the authenticated caller")
			return
		}

		corrID := correlationID(r)

		idempotencyKey := r.Header.Get("Idempotency-Key")
		if idempotencyKey == "" {
			writeWageringError(w, operation.ErrMissingIdempotencyKey, nil)
			return
		}

		result, err := useCase.Process(r.Context(), walletapp.ProcessOperationInput{
			Request: operation.Request{
				ProviderID: req.ProviderID, ExternalTransactionID: req.ExternalTransactionID, PlayerID: req.PlayerID,
				WalletID: req.WalletID, RoundID: req.RoundID, GameID: req.GameID, Kind: req.Kind, Money: req.Money,
				ReferenceExternalTransactionID: req.ReferenceExternalTransactionID,
			},
			IdempotencyKey: idempotencyKey, CorrelationID: corrID, Channel: walletapp.ChannelHTTP,
		})
		if err != nil {
			var opErr *operation.Error
			if errors.As(err, &opErr) {
				writeWageringError(w, opErr, nil)
				return
			}
			logger.Error("process wagering operation failed", "correlationId", corrID, "providerId", req.ProviderID, "walletId", req.WalletID, "error", err)
			writeWageringError(w, operation.ErrTemporarilyUnavailable, nil)
			return
		}

		logger.Info("wagering operation processed", "correlationId", corrID, "transactionId", result.TransactionID, "walletId", req.WalletID, "providerId", req.ProviderID, "status", result.Status)
		status := http.StatusOK
		if result.Status == domainwallet.Rejected {
			status = http.StatusUnprocessableEntity
		} else if result.Status == domainwallet.PendingReference {
			status = http.StatusAccepted
		}
		var balance *money.Money
		if result.Status != domainwallet.PendingReference {
			balance = &result.Balance
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(wageringTransactionResponse{
			TransactionID: result.TransactionID, Status: result.Status, FailureCode: result.FailureCode,
			Balance: balance, PendingExpiresAt: result.PendingExpiresAt, IdempotentReplay: result.IdempotentReplay,
		}); err != nil {
			// Headers and the status line are already written, so this can
			// only be logged, never turned into a different response.
			logger.Error("encode wagering response failed", "correlationId", corrID, "transactionId", result.TransactionID, "walletId", req.WalletID, "providerId", req.ProviderID, "error", err)
		}
	}
}

// classifyWageringDecodeError mirrors classifyDecodeError (wallet.go) with
// this endpoint's own field name for a money-shaped decode failure.
func classifyWageringDecodeError(err error) (*operation.Error, []errorDetailItem) {
	var moneyErr *money.Error
	if errors.As(err, &moneyErr) {
		opErr := walletapp.ClassifyMoneyError(moneyErr)
		return opErr, []errorDetailItem{{Field: "money", Reason: messageForCode(opErr.Code())}}
	}
	return operation.ErrInvalidRequest, []errorDetailItem{{Field: "body", Reason: "malformed JSON, an unknown field, or trailing data after the JSON object"}}
}

// statusForWageringError maps a classified error to the status this
// endpoint's contract reserves for it (spec, "Contratos HTTP": "400
// (formato) / 404 (WALLET_NOT_FOUND) / 422 (demais corrigíveis)"). The
// classification catalog alone cannot tell a format problem apart from any
// other correctable one, so the two format codes - a malformed request
// (INVALID_REQUEST: bad JSON, an unknown field, an invalid UUID) and a
// malformed money value (INVALID_MONEY) - are the only Correctable codes
// singled out before falling through to the classification-driven mapping;
// every other Correctable code (UNSUPPORTED_CURRENCY,
// INVALID_AMOUNT_FOR_KIND, KIND_NOT_ALLOWED, MISSING_IDEMPOTENCY_KEY,
// REFERENCE_NOT_ALLOWED, WALLET_PLAYER_MISMATCH, WALLET_CURRENCY_MISMATCH,
// and REFERENCE_REQUIRED - not in the spec's table, so it follows the same
// "422 for the rest" rule) lands on 422 through that switch.
func statusForWageringError(err *operation.Error) int {
	switch err.Code() {
	case operation.CodeInvalidRequest, operation.CodeInvalidMoney:
		return http.StatusBadRequest
	case operation.CodeWalletNotFound, operation.CodeTransactionNotFound:
		return http.StatusNotFound
	}
	switch err.Classification() {
	case operation.Correctable, operation.Definitive:
		return http.StatusUnprocessableEntity
	case operation.Conflict:
		return http.StatusConflict
	case operation.Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeWageringError(w http.ResponseWriter, err *operation.Error, details []errorDetailItem) {
	if details == nil {
		details = []errorDetailItem{}
	}
	writeErrorEnvelope(w, statusForWageringError(err), errorBody{Error: errorDetail{
		Code:        string(err.Code()),
		Message:     messageForCode(err.Code()),
		Correctable: err.Classification() == operation.Correctable,
		Details:     details,
	}})
}
