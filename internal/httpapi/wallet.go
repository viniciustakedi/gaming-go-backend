package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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

type ledgerEntryResponse struct {
	SequenceNumber int64       `json:"sequenceNumber"`
	TransactionID  string      `json:"transactionId"`
	Direction      string      `json:"direction"`
	Money          money.Money `json:"money"`
	BalanceBefore  money.Money `json:"balanceBefore"`
	BalanceAfter   money.Money `json:"balanceAfter"`
	OccurredAt     time.Time   `json:"occurredAt"`
}

type ledgerResponse struct {
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor *string               `json:"nextCursor"`
}

type reconciliationResponse struct {
	WalletID          string      `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int64       `json:"checkedEntries"`
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

func ledgerHandler(useCase *walletapp.LedgerAuditUseCase, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		walletID := r.PathValue("walletId")
		if _, err := uuid.Parse(walletID); err != nil {
			writeOperationError(w, operation.ErrWalletNotFound, nil)
			return
		}
		after, err := decodeLedgerCursor(r.URL.Query().Get("cursor"))
		if err != nil {
			writeOperationError(w, operation.ErrInvalidRequest, []errorDetailItem{{Field: "cursor", Reason: "must be a valid ledger cursor"}})
			return
		}
		limit, err := ledgerLimit(r.URL.Query().Get("limit"))
		if err != nil {
			writeOperationError(w, operation.ErrInvalidRequest, []errorDetailItem{{Field: "limit", Reason: "must be an integer from 1 to 200"}})
			return
		}
		page, err := useCase.List(r.Context(), walletID, after, limit)
		if err != nil {
			var opErr *operation.Error
			if errors.As(err, &opErr) {
				writeOperationError(w, opErr, nil)
				return
			}
			logger.Error("list ledger failed", "walletId", walletID, "error", err)
			writeOperationError(w, operation.ErrTemporarilyUnavailable, nil)
			return
		}
		response := ledgerResponse{Entries: make([]ledgerEntryResponse, len(page.Entries))}
		for i, entry := range page.Entries {
			response.Entries[i] = ledgerEntryResponse{SequenceNumber: entry.SequenceNumber, TransactionID: entry.TransactionID, Direction: string(entry.Direction), Money: entry.Money, BalanceBefore: entry.BalanceBefore, BalanceAfter: entry.BalanceAfter, OccurredAt: entry.OccurredAt}
		}
		if page.Next != nil {
			cursor := encodeLedgerCursor(*page.Next)
			response.NextCursor = &cursor
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			logger.Error("encode ledger response failed", "walletId", walletID, "error", err)
		}
	}
}

func reconciliationHandler(useCase *walletapp.LedgerAuditUseCase, logger *slog.Logger, metrics *reconciliationMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		walletID := r.PathValue("walletId")
		if _, err := uuid.Parse(walletID); err != nil {
			writeOperationError(w, operation.ErrWalletNotFound, nil)
			return
		}
		result, err := useCase.Reconcile(r.Context(), walletID)
		if err != nil {
			var opErr *operation.Error
			if errors.As(err, &opErr) {
				writeOperationError(w, opErr, nil)
				return
			}
			logger.Error("reconcile wallet failed", "walletId", walletID, "error", err)
			writeOperationError(w, operation.ErrTemporarilyUnavailable, nil)
			return
		}
		consistent, err := result.StoredBalance.Compare(result.CalculatedBalance)
		if err != nil {
			logger.Error("compare reconciliation balances failed", "walletId", walletID, "error", err)
			writeOperationError(w, operation.ErrTemporarilyUnavailable, nil)
			return
		}
		response := reconciliationResponse{WalletID: result.WalletID, StoredBalance: result.StoredBalance, CalculatedBalance: result.CalculatedBalance, Difference: result.Difference, Consistent: consistent == 0, CheckedEntries: result.CheckedEntries}
		if !response.Consistent {
			logger.Warn("wallet reconciliation divergence", "walletId", walletID)
			metrics.divergences.Inc()
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			logger.Error("encode reconciliation response failed", "walletId", walletID, "error", err)
		}
	}
}

func ledgerLimit(raw string) (int, error) {
	if raw == "" {
		return 50, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, errors.New("invalid ledger limit")
	}
	return limit, nil
}

func encodeLedgerCursor(sequence int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(sequence, 10)))
}

func decodeLedgerCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, err
	}
	sequence, err := strconv.ParseInt(string(decoded), 10, 64)
	if err != nil || sequence < 1 {
		return 0, errors.New("invalid ledger cursor")
	}
	return sequence, nil
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
