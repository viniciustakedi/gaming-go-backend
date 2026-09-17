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

// maxOpaqueIdentifierBytes mirrors the schema's own bound on providerId and
// externalTransactionId (migration 0004) - a path segment outside it can
// never match a stored row, so it is rejected as malformed (400) before
// ever reaching the use case.
const maxOpaqueIdentifierBytes = 255

// transactionDetailResponse is the wire shape for both wagering transaction
// read routes. Every key is always present; fields that do not apply to a
// given row - external metadata on the INTERNAL OPENING row,
// resultingBalance outside PROCESSED/REJECTED, nextAttemptAt and
// pendingExpiresAt outside PENDING_REFERENCE - are sent as explicit JSON
// null rather than omitted, so callers can tell "absent" from "not
// applicable to this row".
type transactionDetailResponse struct {
	TransactionID                  string                         `json:"transactionId"`
	ExternalTransactionID          *string                        `json:"externalTransactionId"`
	ProviderID                     *string                        `json:"providerId"`
	PlayerID                       string                         `json:"playerId"`
	WalletID                       string                         `json:"walletId"`
	RoundID                        *string                        `json:"roundId"`
	GameID                         *string                        `json:"gameId"`
	Kind                           domainwallet.WagerKind         `json:"kind"`
	Origin                         domainwallet.TransactionOrigin `json:"origin"`
	Money                          money.Money                    `json:"money"`
	ReferenceExternalTransactionID *string                        `json:"referenceExternalTransactionId"`
	ReferenceTransactionID         *string                        `json:"referenceTransactionId"`
	Status                         domainwallet.TransactionStatus `json:"status"`
	FailureCode                    *string                        `json:"failureCode"`
	ResultingBalance               *money.Money                   `json:"resultingBalance"`
	Attempts                       int                            `json:"attempts"`
	NextAttemptAt                  *time.Time                     `json:"nextAttemptAt"`
	PendingExpiresAt               *time.Time                     `json:"pendingExpiresAt"`
	CreatedAt                      time.Time                      `json:"createdAt"`
	UpdatedAt                      time.Time                      `json:"updatedAt"`
}

// nullableString turns an empty string - this codebase's convention for "not
// applicable to this row" (walletapp.TransactionDetail's doc comment) - into
// a nil pointer, so it serializes as JSON null instead of "".
func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// utcPtr normalizes a nullable timestamp to UTC before serialization. pgx
// hands back a TIMESTAMPTZ in whatever location the driver's default is,
// and json.Marshal renders time.Time with its offset, not necessarily "Z";
// anything but UTC would break the RFC 3339 UTC wire contract.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

func toTransactionDetailResponse(d *walletapp.TransactionDetail) transactionDetailResponse {
	return transactionDetailResponse{
		TransactionID: d.TransactionID, ExternalTransactionID: nullableString(d.ExternalTransactionID), ProviderID: nullableString(d.ProviderID),
		PlayerID: d.PlayerID, WalletID: d.WalletID, RoundID: nullableString(d.RoundID), GameID: nullableString(d.GameID),
		Kind: d.Kind, Origin: d.Origin, Money: d.Money,
		ReferenceExternalTransactionID: nullableString(d.ReferenceExternalTransactionID), ReferenceTransactionID: nullableString(d.ReferenceTransactionID),
		Status: d.Status, FailureCode: nullableString(d.FailureCode), ResultingBalance: d.ResultingBalance,
		Attempts: d.Attempts, NextAttemptAt: utcPtr(d.NextAttemptAt), PendingExpiresAt: utcPtr(d.PendingExpiresAt),
		CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC(),
	}
}

// callerFromIdentity turns the request's verified identity into the
// use case's own notion of caller - wallet-admin sees everything, a
// provider only ever its own.
func callerFromIdentity(identity auth.Identity) walletapp.Caller {
	return walletapp.Caller{ProviderID: identity.ProviderID, IsAdmin: identity.HasRole(auth.RoleWalletAdmin)}
}

// wageringTransactionByIDHandler implements GET /wagering/transactions/{transactionId}.
// The route accepts both provider and wallet-admin (requireAnyRole, wired
// in server.go); this handler asks GetTransactionUseCase to apply the
// per-caller visibility rule the route alone cannot express.
func wageringTransactionByIDHandler(useCase *walletapp.GetTransactionUseCase, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFromContext(r.Context())
		if !ok {
			writeAuthError(w, http.StatusUnauthorized, codeUnauthorized, "missing or malformed bearer token")
			return
		}

		id := r.PathValue("transactionId")
		if !operation.IsCanonicalUUID(id) {
			writeWageringError(w, operation.ErrInvalidRequest, []errorDetailItem{{Field: "transactionId", Reason: "must be a canonical lowercase UUID"}})
			return
		}

		detail, err := useCase.ByID(r.Context(), id, callerFromIdentity(identity))
		if err != nil {
			var opErr *operation.Error
			if errors.As(err, &opErr) {
				writeWageringError(w, opErr, nil)
				return
			}
			logger.Error("get wagering transaction failed", "transactionId", id, "error", err)
			writeWageringError(w, operation.ErrTemporarilyUnavailable, nil)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(toTransactionDetailResponse(detail)); err != nil {
			logger.Error("encode transaction detail response failed", "transactionId", id, "error", err)
		}
	}
}

// providerWageringTransactionHandler implements
// GET /providers/{providerId}/wagering/transactions/{externalTransactionId}.
func providerWageringTransactionHandler(useCase *walletapp.GetTransactionUseCase, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFromContext(r.Context())
		if !ok {
			writeAuthError(w, http.StatusUnauthorized, codeUnauthorized, "missing or malformed bearer token")
			return
		}

		routeProviderID := r.PathValue("providerId")
		externalID := r.PathValue("externalTransactionId")
		if !isValidOpaqueIdentifier(routeProviderID) {
			writeWageringError(w, operation.ErrInvalidRequest, []errorDetailItem{{Field: "providerId", Reason: "must be 1 to 255 bytes"}})
			return
		}
		if !isValidOpaqueIdentifier(externalID) {
			writeWageringError(w, operation.ErrInvalidRequest, []errorDetailItem{{Field: "externalTransactionId", Reason: "must be 1 to 255 bytes"}})
			return
		}

		detail, err := useCase.ByProviderExternalID(r.Context(), routeProviderID, externalID, callerFromIdentity(identity))
		if err != nil {
			if errors.Is(err, walletapp.ErrForbiddenProvider) {
				writeAuthError(w, http.StatusForbidden, codeForbidden, "providerId does not match the authenticated caller")
				return
			}
			var opErr *operation.Error
			if errors.As(err, &opErr) {
				writeWageringError(w, opErr, nil)
				return
			}
			logger.Error("get provider wagering transaction failed", "providerId", routeProviderID, "error", err)
			writeWageringError(w, operation.ErrTemporarilyUnavailable, nil)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(toTransactionDetailResponse(detail)); err != nil {
			logger.Error("encode transaction detail response failed", "providerId", routeProviderID, "error", err)
		}
	}
}

func isValidOpaqueIdentifier(s string) bool {
	return len(s) >= 1 && len(s) <= maxOpaqueIdentifierBytes
}
