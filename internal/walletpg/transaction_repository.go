package walletpg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
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

// Insert writes one wager_transactions row unconditionally. Attempts,
// next_attempt_at and pending_expires_at are left to their column defaults
// (0 / NULL / NULL): this ticket only ever inserts an already-terminal
// OPENING row this way, which never needs any of the three - the reference
// worker (ticket 11) is what first populates them.
func (r *transactionRepository) Insert(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) error {
	fields, err := transactionFields(t, resultingBalance)
	if err != nil {
		return err
	}
	if _, err := r.q.Exec(ctx, insertWagerTransactionSQL, fields...); err != nil {
		return fmt.Errorf("walletpg: insert wager transaction: %w", err)
	}
	return nil
}

// InsertNew is Insert's ON CONFLICT DO NOTHING sibling for an external
// operation, the backstop the spec calls for at decision 3, step 5: a
// concurrent writer whose request body carried a different walletId never
// contends for the same FOR UPDATE lock, so this INSERT - not the lock - is
// what stops it from creating a second row for the same (providerId,
// idempotencyKey) or (providerId, externalTransactionId) pair. inserted is
// false exactly when that backstop fired, telling the caller to roll back
// and reclassify the attempt in a fresh transaction rather than commit
// nothing here and press on inside one whose own INSERT already failed to
// affect a row.
func (r *transactionRepository) InsertNew(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) (bool, error) {
	fields, err := transactionFields(t, resultingBalance)
	if err != nil {
		return false, err
	}
	tag, err := r.q.Exec(ctx, insertWagerTransactionSQL+" ON CONFLICT DO NOTHING", fields...)
	if err != nil {
		return false, fmt.Errorf("walletpg: insert new wager transaction: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const insertWagerTransactionSQL = `
	INSERT INTO wager_transactions (
		id, external_transaction_id, provider_id, idempotency_key, payload_hash,
		wallet_id, player_id, round_id, game_id, kind, origin, amount, currency,
		reference_external_transaction_id, reference_transaction_id, status, failure_code,
		resulting_balance, created_at, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`

// transactionFields builds insertWagerTransactionSQL's positional arguments
// from a domain transaction, mirroring WagerTransaction.validate(): an
// INTERNAL row's provider metadata columns are all NULL, an EXTERNAL row's
// are all populated.
func transactionFields(t *domainwallet.WagerTransaction, resultingBalance *int64) ([]any, error) {
	amount, err := t.Money().MinorUnits()
	if err != nil {
		return nil, fmt.Errorf("walletpg: transaction amount: %w", err)
	}
	currency, err := t.Money().Currency()
	if err != nil {
		return nil, fmt.Errorf("walletpg: transaction currency: %w", err)
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

	return []any{
		t.ID(), externalTransactionID, providerID, idempotencyKey, payloadHash,
		t.WalletID(), t.PlayerID(), roundID, gameID, string(t.Kind()), string(t.Origin()), amount, string(currency),
		referenceExternalID, referenceTransactionID, string(t.Status()), failureCode,
		resultingBalance, t.CreatedAt(), t.UpdatedAt(),
	}, nil
}

// FindByIdempotencyKey and FindByExternalTransactionID both look up an
// EXTERNAL row by one of the two columns idempotency is scoped by provider
// on (spec: "o escopo de chaves é por provedor"); an INTERNAL row's
// provider_id is always NULL, so neither can ever match one.
func (r *transactionRepository) FindByIdempotencyKey(ctx context.Context, providerID, idempotencyKey string) (*walletapp.ExistingTransaction, error) {
	return r.findExisting(ctx, `
		SELECT id, external_transaction_id, idempotency_key, payload_hash, status, failure_code, resulting_balance, currency
		FROM wager_transactions
		WHERE provider_id = $1 AND idempotency_key = $2`, providerID, idempotencyKey)
}

func (r *transactionRepository) FindByExternalTransactionID(ctx context.Context, providerID, externalTransactionID string) (*walletapp.ExistingTransaction, error) {
	return r.findExisting(ctx, `
		SELECT id, external_transaction_id, idempotency_key, payload_hash, status, failure_code, resulting_balance, currency
		FROM wager_transactions
		WHERE provider_id = $1 AND external_transaction_id = $2`, providerID, externalTransactionID)
}

func (r *transactionRepository) findExisting(ctx context.Context, sql, providerID, key string) (*walletapp.ExistingTransaction, error) {
	row := r.q.QueryRow(ctx, sql, providerID, key)

	var (
		id, externalTransactionID, idempotencyKey, payloadHash, status, currency string
		failureCode                                                              *string
		resultingBalance                                                         *int64
	)
	if err := row.Scan(&id, &externalTransactionID, &idempotencyKey, &payloadHash, &status, &failureCode, &resultingBalance, &currency); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, walletapp.ErrNotFound
		}
		return nil, fmt.Errorf("walletpg: find wager transaction: %w", err)
	}

	record := &walletapp.ExistingTransaction{
		TransactionID: id, IdempotencyKey: idempotencyKey, PayloadHash: payloadHash, ExternalTransactionID: externalTransactionID,
		Status: domainwallet.TransactionStatus(status), ResultingBalance: resultingBalance, Currency: money.Currency(currency),
	}
	if failureCode != nil {
		record.FailureCode = *failureCode
	}
	return record, nil
}

// FindReference resolves a REFUND, ROLLBACK or referenced WIN's reference
// by (providerId, referenceExternalTransactionId) (spec, "Regras das
// operações e referências"). It rehydrates the full domain transaction -
// not just the handful of fields operation.Evaluate reads - so a corrupted
// row fails validate() here rather than being silently trusted.
func (r *transactionRepository) FindReference(ctx context.Context, providerID, referenceExternalTransactionID string) (*domainwallet.WagerTransaction, error) {
	row := r.q.QueryRow(ctx, `
		SELECT id, external_transaction_id, provider_id, idempotency_key, payload_hash,
			wallet_id, player_id, round_id, game_id, kind, origin, amount, currency,
			reference_external_transaction_id, reference_transaction_id, status, failure_code,
			created_at, updated_at
		FROM wager_transactions
		WHERE provider_id = $1 AND external_transaction_id = $2`, providerID, referenceExternalTransactionID)

	var (
		id, externalTransactionID, txProviderID, idempotencyKey, payloadHash string
		walletID, playerID, roundID, gameID, kind, origin, currency          string
		amount                                                               int64
		referenceExternalID, referenceTransactionID, failureCode             *string
		status                                                               string
		createdAt, updatedAt                                                 time.Time
	)
	if err := row.Scan(
		&id, &externalTransactionID, &txProviderID, &idempotencyKey, &payloadHash,
		&walletID, &playerID, &roundID, &gameID, &kind, &origin, &amount, &currency,
		&referenceExternalID, &referenceTransactionID, &status, &failureCode, &createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, walletapp.ErrNotFound
		}
		return nil, fmt.Errorf("walletpg: find reference transaction: %w", err)
	}

	amountMoney, err := money.New(amount, money.Currency(currency))
	if err != nil {
		return nil, fmt.Errorf("walletpg: decode reference amount: %w", err)
	}

	input := domainwallet.RehydratedTransaction{
		ID: id, ExternalTransactionID: externalTransactionID, ProviderID: txProviderID,
		IdempotencyKey: idempotencyKey, PayloadHash: payloadHash, WalletID: walletID, PlayerID: playerID,
		RoundID: roundID, GameID: gameID, Kind: domainwallet.WagerKind(kind), Origin: domainwallet.TransactionOrigin(origin),
		Money: amountMoney, Status: domainwallet.TransactionStatus(status), CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	if referenceExternalID != nil {
		input.ReferenceExternalTransactionID = *referenceExternalID
	}
	if referenceTransactionID != nil {
		input.ReferenceTransactionID = *referenceTransactionID
	}
	if failureCode != nil {
		input.FailureCode = *failureCode
	}

	transaction, err := domainwallet.RehydrateTransaction(input)
	if err != nil {
		return nil, fmt.Errorf("walletpg: rehydrate reference transaction: %w", err)
	}
	return transaction, nil
}

// ExistsSuccessfulReversal backs the partial unique index's application-side
// counterpart (spec, migration 0004: "wager_transactions_reversal_per_reference_idx").
// It is only ever queried while this call still holds the referenced
// transaction's wallet FOR UPDATE lock, so a concurrent REFUND and ROLLBACK
// of the same BET can never both observe false: the second one always runs
// after the first's commit has become visible.
func (r *transactionRepository) ExistsSuccessfulReversal(ctx context.Context, referenceTransactionID string) (bool, error) {
	var exists bool
	if err := r.q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM wager_transactions
			WHERE reference_transaction_id = $1 AND kind IN ('REFUND', 'ROLLBACK') AND status = 'PROCESSED'
		)`, referenceTransactionID).Scan(&exists); err != nil {
		return false, fmt.Errorf("walletpg: check successful reversal: %w", err)
	}
	return exists, nil
}

func strPtr(s string) *string { return &s }
