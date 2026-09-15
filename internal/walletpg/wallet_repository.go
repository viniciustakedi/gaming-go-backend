// Package walletpg is the Postgres adapter for the wallet application
// layer (internal/walletapp): pgx-backed implementations of its repository
// ports and unit of work, plus the Fx module that wires them and the use
// cases built from them into the composition root.
package walletpg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/pg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

// sqlstateUniqueViolation is spelled out by hand rather than imported from
// pgerrcode: decision 8 of the alignment restricts this repository's
// dependencies, and pgerrcode is not among them - the same choice
// test/integration/helpers_test.go already made.
const sqlstateUniqueViolation = "23505"

// walletRepository is bound to one pg.Querier for its whole lifetime: the
// pool itself, for the read-only instance internal/walletapp.
// GetWalletUseCase uses, or one transaction's Querier, for the instance a
// unitOfWork builds fresh inside WithinTx. internal/walletapp never sees
// this binding - it only ever sees the walletapp.WalletRepository port.
type walletRepository struct{ q pg.Querier }

// newWalletRepository builds the pgx-backed WalletRepository bound to q.
func newWalletRepository(q pg.Querier) walletapp.WalletRepository {
	return &walletRepository{q: q}
}

func (r *walletRepository) Insert(ctx context.Context, w *domainwallet.Wallet) error {
	balance, err := w.Balance().MinorUnits()
	if err != nil {
		return fmt.Errorf("walletpg: wallet balance: %w", err)
	}
	currency, err := w.Balance().Currency()
	if err != nil {
		return fmt.Errorf("walletpg: wallet currency: %w", err)
	}

	_, err = r.q.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID(), w.PlayerID(), string(currency), balance, w.Version(), w.CreatedAt(), w.UpdatedAt())
	if err != nil {
		var pgErr *pgconn.PgError
		// This is the only backstop against a duplicate wallet: the
		// application never SELECTs before INSERTing, so two concurrent
		// opens for the same (playerId, currency) both reach this Exec, and
		// only the one Postgres serializes first wins - the other gets this
		// exact violation, translated to a conflict the use case maps to
		// WALLET_ALREADY_EXISTS (spec: "uma segunda carteira ... mesmo sob
		// aberturas concorrentes").
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateUniqueViolation && pgErr.ConstraintName == "wallets_player_id_currency_key" {
			return walletapp.ErrAlreadyExists
		}
		return fmt.Errorf("walletpg: insert wallet: %w", err)
	}
	return nil
}

func (r *walletRepository) FindByID(ctx context.Context, id string) (*domainwallet.Wallet, error) {
	return r.find(ctx, `
		SELECT id, player_id, currency, balance, version, created_at, updated_at
		FROM wallets
		WHERE id = $1`, id)
}

// FindForUpdate is FindByID's exact query plus FOR UPDATE, so the caller
// holds an exclusive row lock on the wallet for the rest of its transaction
// (spec, decision 3, step 2: "SELECT ... FOR UPDATE na linha da carteira").
// Only ever called against a transaction-bound Querier - locking through the
// bare pool would release the lock the instant this single statement's
// implicit transaction ends.
func (r *walletRepository) FindForUpdate(ctx context.Context, id string) (*domainwallet.Wallet, error) {
	return r.find(ctx, `
		SELECT id, player_id, currency, balance, version, created_at, updated_at
		FROM wallets
		WHERE id = $1
		FOR UPDATE`, id)
}

func (r *walletRepository) find(ctx context.Context, sql string, id string) (*domainwallet.Wallet, error) {
	row := r.q.QueryRow(ctx, sql, id)

	var (
		walletID, playerID, currency string
		balance, version             int64
		createdAt, updatedAt         time.Time
	)
	if err := row.Scan(&walletID, &playerID, &currency, &balance, &version, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, walletapp.ErrNotFound
		}
		return nil, fmt.Errorf("walletpg: find wallet: %w", err)
	}

	balanceValue, err := money.New(balance, money.Currency(currency))
	if err != nil {
		return nil, fmt.Errorf("walletpg: decode wallet balance: %w", err)
	}

	return domainwallet.Rehydrate(domainwallet.RehydratedWallet{
		ID: walletID, PlayerID: playerID, Currency: money.Currency(currency), Balance: balanceValue,
		Version: version, CreatedAt: createdAt, UpdatedAt: updatedAt,
	})
}

// UpdateBalance persists w's current balance and version, conditioned on
// previousVersion so a writer that no longer holds the row's lock can never
// silently overwrite a newer state (spec: "UPDATE da carteira com WHERE
// version = <lida>" - defense in depth alongside the FOR UPDATE lock, not a
// substitute for it).
func (r *walletRepository) UpdateBalance(ctx context.Context, w *domainwallet.Wallet, previousVersion int64) error {
	balance, err := w.Balance().MinorUnits()
	if err != nil {
		return fmt.Errorf("walletpg: wallet balance: %w", err)
	}

	tag, err := r.q.Exec(ctx, `
		UPDATE wallets
		SET balance = $1, version = $2, updated_at = $3
		WHERE id = $4 AND version = $5`,
		balance, w.Version(), w.UpdatedAt(), w.ID(), previousVersion)
	if err != nil {
		return fmt.Errorf("walletpg: update wallet balance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return walletapp.ErrConcurrencyConflict
	}
	return nil
}
