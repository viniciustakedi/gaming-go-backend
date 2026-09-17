package walletapp

import (
	"context"
	"errors"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

// OpenWalletInput is everything the use case needs beyond what it generates
// itself (ids, timestamps).
type OpenWalletInput struct {
	PlayerID       string
	InitialBalance money.Money
	// CorrelationID ties the two outbox events this call may write back to
	// the HTTP request that caused them.
	CorrelationID string
}

// OpenWalletUseCase opens a player's wallet in one currency, crediting an
// optional positive initial balance in the same commit as its OPENING
// transaction, ledger entry and outbox events.
type OpenWalletUseCase struct {
	uow UnitOfWork
	now func() time.Time
}

// NewOpenWalletUseCase wires the use case to its unit of work. now defaults
// to time.Now; tests substitute a fixed clock.
func NewOpenWalletUseCase(uow UnitOfWork) *OpenWalletUseCase {
	return &OpenWalletUseCase{uow: uow, now: time.Now}
}

// Open builds the wallet (and, for a positive initial balance, its OPENING
// transaction, ledger entry and two outbox events) in memory, then persists
// all of it in a single transaction. A second wallet for the same player and
// currency - even opened concurrently - fails at the database's own unique
// constraint, translated here to operation.ErrWalletAlreadyExists; any other
// write failure is returned unclassified, for the caller to treat as
// transient.
func (uc *OpenWalletUseCase) Open(ctx context.Context, input OpenWalletInput) (*domainwallet.Wallet, error) {
	currency, err := input.InitialBalance.Currency()
	if err != nil {
		return nil, ClassifyMoneyError(err)
	}

	now := uc.now().UTC()

	walletID, err := newID()
	if err != nil {
		return nil, err
	}

	zero, err := money.Zero(currency)
	if err != nil {
		return nil, ClassifyMoneyError(err)
	}
	// Parse never lets a negative amount reach here, but Compare's contract
	// still has to be checked; Open below re-validates non-negativity anyway.
	comparison, err := input.InitialBalance.Compare(zero)
	if err != nil {
		return nil, ClassifyMoneyError(err)
	}

	var openingTransactionID, ledgerEntryID string
	if comparison > 0 {
		if openingTransactionID, err = newID(); err != nil {
			return nil, err
		}
		if ledgerEntryID, err = newID(); err != nil {
			return nil, err
		}
	}

	walletValue, entry, err := domainwallet.Open(domainwallet.OpenInput{
		ID:                   walletID,
		PlayerID:             input.PlayerID,
		Currency:             currency,
		InitialBalance:       input.InitialBalance,
		OpeningTransactionID: openingTransactionID,
		LedgerEntryID:        ledgerEntryID,
		OccurredAt:           now,
	})
	if err != nil {
		return nil, operation.ErrInvalidRequest
	}

	var transaction *domainwallet.WagerTransaction
	if entry != nil {
		transaction, err = domainwallet.NewOpeningTransaction(domainwallet.OpeningTransactionInput{
			ID: openingTransactionID, WalletID: walletID, PlayerID: input.PlayerID, Money: input.InitialBalance, CreatedAt: now,
		})
		if err != nil {
			return nil, operation.ErrInvalidRequest
		}
		if err := transaction.MarkProcessed(now); err != nil {
			return nil, operation.ErrInvalidRequest
		}
	}

	metadata := func() (domainwallet.EventMetadata, error) {
		eventID, err := newID()
		if err != nil {
			return domainwallet.EventMetadata{}, err
		}
		return domainwallet.EventMetadata{EventID: eventID, CorrelationID: input.CorrelationID, OccurredAt: now}, nil
	}

	err = uc.uow.WithinTx(ctx, func(ctx context.Context, repos Repositories) error {
		if err := repos.Wallets.Insert(ctx, walletValue); err != nil {
			return err
		}
		if entry == nil {
			return nil
		}

		resultingBalance, err := walletValue.Balance().MinorUnits()
		if err != nil {
			return err
		}
		if err := repos.Transactions.Insert(ctx, transaction, &resultingBalance); err != nil {
			return err
		}
		if err := repos.Ledger.Insert(ctx, entry); err != nil {
			return err
		}

		processedMetadata, err := metadata()
		if err != nil {
			return err
		}
		processedEvent, err := domainwallet.NewWagerTransactionProcessed(processedMetadata, transaction)
		if err != nil {
			return err
		}
		if err := repos.Outbox.Insert(ctx, OutboxRecord{
			EventID: processedEvent.EventID, EventType: processedEvent.EventType, AggregateType: "WagerTransaction",
			AggregateID: processedEvent.AggregateID, EventVersion: processedEvent.Version, OccurredAt: now, Payload: processedEvent,
		}); err != nil {
			return err
		}

		balanceMetadata, err := metadata()
		if err != nil {
			return err
		}
		balanceEvent, err := domainwallet.NewWalletBalanceChanged(balanceMetadata, walletValue, entry)
		if err != nil {
			return err
		}
		if err := repos.Outbox.Insert(ctx, OutboxRecord{
			EventID: balanceEvent.EventID, EventType: balanceEvent.EventType, AggregateType: "Wallet",
			AggregateID: balanceEvent.AggregateID, EventVersion: balanceEvent.Version, OccurredAt: now, Payload: balanceEvent,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return nil, operation.ErrWalletAlreadyExists
		}
		return nil, err
	}

	return walletValue, nil
}
