package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

type openWalletRequest struct {
	PlayerID       string      `json:"playerId"`
	InitialBalance money.Money `json:"initialBalance"`
}

type walletResponse struct {
	ID       string      `json:"id"`
	PlayerID string      `json:"playerId"`
	Balance  money.Money `json:"balance"`
	Version  int64       `json:"version"`
}

func toWalletResponse(w *domainwallet.Wallet) walletResponse {
	return walletResponse{ID: w.ID(), PlayerID: w.PlayerID(), Balance: w.Balance(), Version: w.Version()}
}

// openWalletHandler implements POST /wallets. Every corrigible input
// problem this ticket lists - malformed JSON, unknown fields, an
// out-of-format or out-of-allowlist money, a malformed playerId - answers
// 400 with a stable code, and nothing is persisted for any of them, since
// they are all caught before the use case ever opens a transaction.
func openWalletHandler(useCase *walletapp.OpenWalletUseCase, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req openWalletRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			opErr, details := classifyDecodeError(err)
			writeOperationError(w, opErr, details)
			return
		}
		if _, err := uuid.Parse(req.PlayerID); err != nil {
			writeOperationError(w, operation.ErrInvalidRequest, []errorDetailItem{{Field: "playerId", Reason: "must be a valid UUID"}})
			return
		}

		walletValue, err := useCase.Open(r.Context(), walletapp.OpenWalletInput{
			PlayerID:       req.PlayerID,
			InitialBalance: req.InitialBalance,
			CorrelationID:  correlationID(r),
		})
		if err != nil {
			var opErr *operation.Error
			if errors.As(err, &opErr) {
				writeOperationError(w, opErr, detailsForOperationError(opErr))
				return
			}
			logger.Error("open wallet failed", "playerId", req.PlayerID, "error", err)
			writeOperationError(w, operation.ErrTemporarilyUnavailable, nil)
			return
		}

		logger.Info("wallet opened", "walletId", walletValue.ID(), "playerId", walletValue.PlayerID())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(toWalletResponse(walletValue))
	}
}

// getWalletHandler implements GET /wallets/{walletId}.
func getWalletHandler(useCase *walletapp.GetWalletUseCase, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("walletId")
		if _, err := uuid.Parse(id); err != nil {
			writeOperationError(w, operation.ErrWalletNotFound, nil)
			return
		}

		walletValue, err := useCase.Get(r.Context(), id)
		if err != nil {
			var opErr *operation.Error
			if errors.As(err, &opErr) {
				writeOperationError(w, opErr, nil)
				return
			}
			logger.Error("get wallet failed", "walletId", id, "error", err)
			writeOperationError(w, operation.ErrTemporarilyUnavailable, nil)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(toWalletResponse(walletValue))
	}
}

// classifyDecodeError distinguishes a money-shaped decode failure (the
// Money type's own UnmarshalJSON error, propagated unwrapped by
// encoding/json through the outer struct) from every other malformed-body
// shape, which is a generic INVALID_REQUEST, and names the field each
// traces back to.
func classifyDecodeError(err error) (*operation.Error, []errorDetailItem) {
	var moneyErr *money.Error
	if errors.As(err, &moneyErr) {
		opErr := walletapp.ClassifyMoneyError(moneyErr)
		return opErr, []errorDetailItem{{Field: "initialBalance", Reason: messageForCode(opErr.Code())}}
	}
	return operation.ErrInvalidRequest, []errorDetailItem{{Field: "body", Reason: "malformed JSON, an unknown field, or trailing data after the JSON object"}}
}

// correlationID returns the caller-supplied X-Correlation-Id header, or a
// freshly generated one when absent (spec: "correlationId vem do header
// X-Correlation-Id ou é gerado no HTTP").
func correlationID(r *http.Request) string {
	if v := r.Header.Get("X-Correlation-Id"); v != "" {
		return v
	}
	return uuid.NewString()
}
