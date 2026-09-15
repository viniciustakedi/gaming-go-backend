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

func (r *transactionRepository) InsertPending(ctx context.Context, t *domainwallet.WagerTransaction, nextAttemptAt time.Time, ttl time.Duration) (time.Time, bool, error) {
	fields, err := transactionFields(t, nil)
	if err != nil {
		return time.Time{}, false, err
	}
	fields = append(fields, nextAttemptAt, ttl.String())
	var expiresAt time.Time
	if err := r.q.QueryRow(ctx, insertPendingWagerTransactionSQL+" ON CONFLICT DO NOTHING RETURNING pending_expires_at", fields...).Scan(&expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("walletpg: insert pending wager transaction: %w", err)
	}
	return expiresAt, true, nil
}

const insertWagerTransactionSQL = `
	INSERT INTO wager_transactions (
		id, external_transaction_id, provider_id, idempotency_key, payload_hash,
		wallet_id, player_id, round_id, game_id, kind, origin, amount, currency,
		reference_external_transaction_id, reference_transaction_id, status, failure_code,
		resulting_balance, created_at, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`

const insertPendingWagerTransactionSQL = `
	INSERT INTO wager_transactions (
		id, external_transaction_id, provider_id, idempotency_key, payload_hash,
		wallet_id, player_id, round_id, game_id, kind, origin, amount, currency,
		reference_external_transaction_id, reference_transaction_id, status, failure_code,
		resulting_balance, created_at, updated_at, next_attempt_at, pending_expires_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,now() + $22::interval)`

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
		SELECT id, external_transaction_id, idempotency_key, payload_hash, status, failure_code, resulting_balance, currency, pending_expires_at
		FROM wager_transactions
		WHERE provider_id = $1 AND idempotency_key = $2`, providerID, idempotencyKey)
}

func (r *transactionRepository) FindByExternalTransactionID(ctx context.Context, providerID, externalTransactionID string) (*walletapp.ExistingTransaction, error) {
	return r.findExisting(ctx, `
		SELECT id, external_transaction_id, idempotency_key, payload_hash, status, failure_code, resulting_balance, currency, pending_expires_at
		FROM wager_transactions
		WHERE provider_id = $1 AND external_transaction_id = $2`, providerID, externalTransactionID)
}

func (r *transactionRepository) findExisting(ctx context.Context, sql, providerID, key string) (*walletapp.ExistingTransaction, error) {
	row := r.q.QueryRow(ctx, sql, providerID, key)

	var (
		id, externalTransactionID, idempotencyKey, payloadHash, status, currency string
		failureCode                                                              *string
		resultingBalance                                                         *int64
		pendingExpiresAt                                                         *time.Time
	)
	if err := row.Scan(&id, &externalTransactionID, &idempotencyKey, &payloadHash, &status, &failureCode, &resultingBalance, &currency, &pendingExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, walletapp.ErrNotFound
		}
		return nil, fmt.Errorf("walletpg: find wager transaction: %w", err)
	}

	record := &walletapp.ExistingTransaction{
		TransactionID: id, IdempotencyKey: idempotencyKey, PayloadHash: payloadHash, ExternalTransactionID: externalTransactionID,
		Status: domainwallet.TransactionStatus(status), ResultingBalance: resultingBalance, Currency: money.Currency(currency), PendingExpiresAt: pendingExpiresAt,
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
		return nil, fmt.Errorf("walletpg: rehydrate reference transaction: %w: %w", walletapp.ErrInvalidPersistedTransaction, err)
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

func (r *transactionRepository) FindPendingForUpdate(ctx context.Context, transactionID string) (*walletapp.PendingReferenceTransaction, error) {
	row := r.q.QueryRow(ctx, `
		SELECT id, external_transaction_id, provider_id, idempotency_key, payload_hash,
			wallet_id, player_id, round_id, game_id, kind, origin, amount, currency,
			reference_external_transaction_id, reference_transaction_id, status, failure_code,
			attempts, pending_expires_at, pending_expires_at <= now(), created_at, updated_at
		FROM wager_transactions WHERE id = $1 FOR UPDATE`, transactionID)
	var (
		id, externalID, providerID, idempotencyKey, payloadHash     string
		walletID, playerID, roundID, gameID, kind, origin, currency string
		amount                                                      int64
		referenceExternalID, referenceTransactionID, failureCode    *string
		status                                                      string
		attempts                                                    int
		expiresAt, createdAt, updatedAt                             time.Time
		expired                                                     bool
	)
	if err := row.Scan(&id, &externalID, &providerID, &idempotencyKey, &payloadHash,
		&walletID, &playerID, &roundID, &gameID, &kind, &origin, &amount, &currency,
		&referenceExternalID, &referenceTransactionID, &status, &failureCode,
		&attempts, &expiresAt, &expired, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, walletapp.ErrNotFound
		}
		return nil, fmt.Errorf("walletpg: lock pending wager transaction: %w", err)
	}
	value, err := money.New(amount, money.Currency(currency))
	if err != nil {
		return nil, fmt.Errorf("walletpg: decode pending transaction amount: %w", err)
	}
	input := domainwallet.RehydratedTransaction{ID: id, ExternalTransactionID: externalID, ProviderID: providerID, IdempotencyKey: idempotencyKey, PayloadHash: payloadHash, WalletID: walletID, PlayerID: playerID, RoundID: roundID, GameID: gameID, Kind: domainwallet.WagerKind(kind), Origin: domainwallet.TransactionOrigin(origin), Money: value, Status: domainwallet.TransactionStatus(status), CreatedAt: createdAt, UpdatedAt: updatedAt}
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
		return nil, fmt.Errorf("walletpg: rehydrate pending transaction: %w: %w", walletapp.ErrInvalidPersistedTransaction, err)
	}
	return &walletapp.PendingReferenceTransaction{Transaction: transaction, Attempts: attempts, PendingExpiresAt: expiresAt, Expired: expired}, nil
}

func (r *transactionRepository) ReschedulePending(ctx context.Context, transactionID string, attempts int, retryDelay time.Duration) error {
	tag, err := r.q.Exec(ctx, `UPDATE wager_transactions SET attempts = $2, next_attempt_at = now() + $3::interval, updated_at = now() WHERE id = $1 AND status = 'PENDING_REFERENCE'`, transactionID, attempts, retryDelay.String())
	if err != nil {
		return fmt.Errorf("walletpg: reschedule pending wager transaction: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return walletapp.ErrNotFound
	}
	return nil
}

func (r *transactionRepository) CompletePending(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) error {
	if t == nil {
		return errors.New("walletpg: complete nil pending transaction")
	}
	var referenceID, failureCode *string
	if t.ReferenceTransactionID() != "" {
		referenceID = strPtr(t.ReferenceTransactionID())
	}
	if t.FailureCode() != "" {
		failureCode = strPtr(t.FailureCode())
	}
	tag, err := r.q.Exec(ctx, `
		UPDATE wager_transactions
		SET reference_transaction_id = $2, status = $3, failure_code = $4,
			resulting_balance = $5, next_attempt_at = NULL, pending_expires_at = NULL, updated_at = $6
		WHERE id = $1 AND status = 'PENDING_REFERENCE'`, t.ID(), referenceID, t.Status(), failureCode, resultingBalance, t.UpdatedAt())
	if err != nil {
		return fmt.Errorf("walletpg: complete pending wager transaction: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return walletapp.ErrNotFound
	}
	return nil
}

// transactionDetailColumns backs both FindDetailByID and
// FindDetailByProviderExternalID: the full row the spec's "Contratos HTTP"
// registro completo needs, including the attempts/next_attempt_at/
// pending_expires_at columns no domain rehydration path reads yet (they
// exist for ticket 11's worker) - this ticket reads them as plain columns
// instead of going through domainwallet.RehydrateTransaction, which does
// not expose them.
const transactionDetailColumns = `
	id, external_transaction_id, provider_id, player_id, wallet_id, round_id, game_id,
	kind, origin, amount, currency, reference_external_transaction_id, reference_transaction_id,
	status, failure_code, resulting_balance, attempts, next_attempt_at, pending_expires_at,
	created_at, updated_at`

func (r *transactionRepository) FindDetailByID(ctx context.Context, id string) (*walletapp.TransactionDetail, error) {
	row := r.q.QueryRow(ctx, "SELECT "+transactionDetailColumns+" FROM wager_transactions WHERE id = $1", id)
	detail, err := scanTransactionDetail(row)
	if err != nil {
		return nil, fmt.Errorf("walletpg: find wager transaction detail by id: %w", err)
	}
	return detail, nil
}

func (r *transactionRepository) FindDetailByProviderExternalID(ctx context.Context, providerID, externalTransactionID string) (*walletapp.TransactionDetail, error) {
	row := r.q.QueryRow(ctx, "SELECT "+transactionDetailColumns+" FROM wager_transactions WHERE provider_id = $1 AND external_transaction_id = $2", providerID, externalTransactionID)
	detail, err := scanTransactionDetail(row)
	if err != nil {
		return nil, fmt.Errorf("walletpg: find wager transaction detail by external id: %w", err)
	}
	return detail, nil
}

// scanTransactionDetail unmarshals one transactionDetailColumns row.
// ErrNotFound is returned bare, not wrapped, so both callers above can
// still wrap it with their own context while errors.Is(_, walletapp.ErrNotFound)
// keeps working for the use case.
func scanTransactionDetail(row pgx.Row) (*walletapp.TransactionDetail, error) {
	var (
		id, playerID, walletID, kind, origin, currency, status   string
		externalTransactionID, providerID, roundID, gameID       *string
		referenceExternalID, referenceTransactionID, failureCode *string
		amount                                                   int64
		resultingBalance                                         *int64
		attempts                                                 int
		nextAttemptAt, pendingExpiresAt                          *time.Time
		createdAt, updatedAt                                     time.Time
	)
	if err := row.Scan(
		&id, &externalTransactionID, &providerID, &playerID, &walletID, &roundID, &gameID,
		&kind, &origin, &amount, &currency, &referenceExternalID, &referenceTransactionID,
		&status, &failureCode, &resultingBalance, &attempts, &nextAttemptAt, &pendingExpiresAt,
		&createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, walletapp.ErrNotFound
		}
		return nil, err
	}

	amountMoney, err := money.New(amount, money.Currency(currency))
	if err != nil {
		return nil, fmt.Errorf("decode transaction amount: %w", err)
	}

	detail := &walletapp.TransactionDetail{
		TransactionID: id, PlayerID: playerID, WalletID: walletID,
		Kind: domainwallet.WagerKind(kind), Origin: domainwallet.TransactionOrigin(origin), Money: amountMoney,
		Status: domainwallet.TransactionStatus(status), Attempts: attempts, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	if externalTransactionID != nil {
		detail.ExternalTransactionID = *externalTransactionID
	}
	if providerID != nil {
		detail.ProviderID = *providerID
	}
	if roundID != nil {
		detail.RoundID = *roundID
	}
	if gameID != nil {
		detail.GameID = *gameID
	}
	if referenceExternalID != nil {
		detail.ReferenceExternalTransactionID = *referenceExternalID
	}
	if referenceTransactionID != nil {
		detail.ReferenceTransactionID = *referenceTransactionID
	}
	if failureCode != nil {
		detail.FailureCode = *failureCode
	}
	if resultingBalance != nil {
		balanceMoney, err := money.New(*resultingBalance, money.Currency(currency))
		if err != nil {
			return nil, fmt.Errorf("decode transaction resulting balance: %w", err)
		}
		detail.ResultingBalance = &balanceMoney
	}
	detail.NextAttemptAt = nextAttemptAt
	detail.PendingExpiresAt = pendingExpiresAt
	return detail, nil
}

func strPtr(s string) *string { return &s }
