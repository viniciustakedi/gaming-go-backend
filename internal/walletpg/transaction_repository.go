package walletpg

import (
	"context"
	"fmt"

	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/pg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

type transactionRepository struct{ q pg.Querier }

// newWagerTransactionRepository builds the pgx-backed
// WagerTransactionRepository bound to q.
func newWagerTransactionRepository(q pg.Querier) walletapp.WagerTransactionRepository {
	return &transactionRepository{q: q}
}

// Insert writes one wager_transactions row. Attempts, next_attempt_at and
// pending_expires_at are left to their column defaults (0 / NULL / NULL):
// this ticket only ever inserts an already-terminal OPENING row, which
// never needs any of the three - the reference worker (ticket 11) is what
// first populates them.
func (r *transactionRepository) Insert(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) error {
	amount, err := t.Money().MinorUnits()
	if err != nil {
		return fmt.Errorf("walletpg: transaction amount: %w", err)
	}
	currency, err := t.Money().Currency()
	if err != nil {
		return fmt.Errorf("walletpg: transaction currency: %w", err)
	}

	var (
		externalTransactionID, providerID, idempotencyKey, payloadHash *string
		roundID, gameID, referenceExternalID                           *string
		referenceTransactionID                                         *string
		failureCode                                                    *string
	)
	if t.Origin() == domainwallet.External {
		externalTransactionID = strPtr(t.ExternalTransactionID())
		providerID = strPtr(t.ProviderID())
		idempotencyKey = strPtr(t.IdempotencyKey())
		payloadHash = strPtr(t.PayloadHash())
		roundID = strPtr(t.RoundID())
		gameID = strPtr(t.GameID())
	}
	if t.ReferenceExternalTransactionID() != "" {
		referenceExternalID = strPtr(t.ReferenceExternalTransactionID())
	}
	if t.ReferenceTransactionID() != "" {
		referenceTransactionID = strPtr(t.ReferenceTransactionID())
	}
	if t.FailureCode() != "" {
		failureCode = strPtr(t.FailureCode())
	}

	_, err = r.q.Exec(ctx, `
		INSERT INTO wager_transactions (
			id, external_transaction_id, provider_id, idempotency_key, payload_hash,
			wallet_id, player_id, round_id, game_id, kind, origin, amount, currency,
			reference_external_transaction_id, reference_transaction_id, status, failure_code,
			resulting_balance, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		t.ID(), externalTransactionID, providerID, idempotencyKey, payloadHash,
		t.WalletID(), t.PlayerID(), roundID, gameID, string(t.Kind()), string(t.Origin()), amount, string(currency),
		referenceExternalID, referenceTransactionID, string(t.Status()), failureCode,
		resultingBalance, t.CreatedAt(), t.UpdatedAt())
	if err != nil {
		return fmt.Errorf("walletpg: insert wager transaction: %w", err)
	}
	return nil
}

func strPtr(s string) *string { return &s }
