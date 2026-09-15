package walletapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

// Channel labels the transport an operation arrived on, for metrics and
// logging that must tell HTTP and SQS attempts apart even though both call
// the exact same use case (spec: "operações por canal").
const (
	ChannelHTTP = "HTTP"
	ChannelSQS  = "SQS"
)

// ErrRetryInNewTransaction signals that the wager-transaction INSERT found
// nothing to insert (spec, decision 3, step 5's ON CONFLICT DO NOTHING
// backstop): a concurrent writer whose request body carried a different
// walletId, and so never contended for the same FOR UPDATE lock, already
// committed the same (providerId, idempotencyKey) or (providerId,
// externalTransactionId) pair. ExecuteInTx never retries this itself - the
// winner's commit is only visible once this transaction is gone, so
// whoever owns the transaction (Process, below, for HTTP; the SQS
// consumer's inbox transaction, in ticket 13) must roll it back and call
// ExecuteInTx again in a brand new one. Exported so both callers can
// recognise it with errors.Is.
var ErrRetryInNewTransaction = errors.New("walletapp: wager insert conflicted, retry in a new transaction")

// ErrCorruptedResultingBalance is returned when a persisted resulting
// balance can no longer be rebuilt into a valid money.Money for its stored
// currency. This is a data integrity violation, never a provider input
// problem, so it is propagated rather than silently answered with a
// stable-looking but bogus zero balance.
var ErrCorruptedResultingBalance = errors.New("walletapp: persisted balance is not a valid amount for its currency")

// ErrInvalidPersistedTransaction marks a row that cannot be rehydrated into a
// valid domain transaction. A worker must fail this explicitly corrupt state,
// while all unclassified infrastructure errors remain retryable.
var ErrInvalidPersistedTransaction = errors.New("walletapp: persisted transaction violates invariants")

// ErrOperationNotPrepared is returned when ExecuteInTx is handed a
// PreparedOperation that Prepare did not produce - the zero value, above
// all, since PreparedOperation exposes no exported fields and no exported
// constructor other than Prepare. ExecuteInTx checks this before touching
// any repository, so a caller that skips Prepare gets a classified error
// instead of an arbitrary hash and decision reaching the database.
var ErrOperationNotPrepared = errors.New("walletapp: PreparedOperation was not produced by Prepare")

// ProcessOperationInput is everything ProcessOperationUseCase needs beyond
// what it generates itself (transaction and ledger entry ids, timestamps).
type ProcessOperationInput struct {
	Request        operation.Request
	IdempotencyKey string
	// CorrelationID ties every outbox event this call writes back to the
	// request or message that caused them (spec: "correlationId vem do
	// header X-Correlation-Id ou é gerado no HTTP; no SQS, é o messageId").
	CorrelationID string
	// CausationID identifies the command that directly caused emitted events.
	// HTTP has none; the SQS adapter supplies its messageId.
	CausationID string
	Channel     string
}

// ProcessOperationResult is the stable, replay-safe answer the provider
// contract promises: transaction id, terminal status, failure code when
// rejected, the balance observed at that exact processing, and whether this
// answer came from a fresh attempt or a replay of one already persisted.
type ProcessOperationResult struct {
	TransactionID    string
	Status           domainwallet.TransactionStatus
	FailureCode      string
	Balance          money.Money
	PendingExpiresAt *time.Time
	IdempotentReplay bool
	kind             domainwallet.WagerKind
}

// PendingResumeSettings controls one durable pending-reference retry. The
// worker supplies these values from configuration, keeping HTTP/SQS request
// processing free from polling concerns.
type PendingResumeSettings struct {
	MaxAttempts int
	RetryDelay  time.Duration
}

type PendingResumeOutcome string

const (
	PendingRescheduled PendingResumeOutcome = "rescheduled"
	PendingProcessed   PendingResumeOutcome = "processed"
	PendingRejected    PendingResumeOutcome = "rejected"
)

// PreparedOperation is Prepare's result: an input already validated and
// hashed, ready for ExecuteInTx to run against a transaction's repositories
// without repeating either step (spec, decision 3, step 1: "validar e
// calcular o hash fora da transação"). Ticket 13's SQS consumer builds one
// of these outside its inbox transaction, exactly as Process does below for
// HTTP, then hands it to ExecuteInTx inside that transaction.
//
// Every field is unexported and there is no exported constructor other than
// Prepare, so a PreparedOperation carrying a hash or decision ExecuteInTx
// did not itself produce cannot be assembled outside this package - by
// struct literal or otherwise. ExecuteInTx also rejects the zero value (see
// ready, below) rather than trust an unprepared or forged one.
type PreparedOperation struct {
	input    ProcessOperationInput
	hash     string
	decision operation.Decision
	// ready is set only by Prepare. Its zero value, false, is what every
	// PreparedOperation{} literal built outside this package carries, so
	// ExecuteInTx uses it to reject anything Prepare did not produce.
	ready bool
}

// Hash returns the canonical PayloadHash Prepare computed for this
// operation, for callers (tests, mainly) that need to read what Prepare
// decided without being able to construct or mutate a PreparedOperation.
func (p PreparedOperation) Hash() string {
	return p.hash
}

// Decision returns the domain decision Prepare evaluated for this
// operation.
func (p PreparedOperation) Decision() operation.Decision {
	return p.decision
}

// ProcessOperationUseCase is the shared processing use case HTTP and SQS
// both call (spec: "um caso de uso de processamento compartilhado"). It
// implements decision 3's concurrency flow in full: validate and hash
// outside any transaction, lock the wallet row, classify the attempt against
// whatever is already committed, insert with ON CONFLICT DO NOTHING and
// reclassify in a fresh transaction if that finds nothing to insert, then
// apply the movement, the ledger entry, the wallet update and the outbox
// records in the one transaction that commits.
type ProcessOperationUseCase struct {
	uow        UnitOfWork
	metrics    OperationMetrics
	now        func() time.Time
	pendingTTL time.Duration
}

// NewProcessOperationUseCase wires the use case to its unit of work and
// metrics port. now defaults to time.Now; tests substitute a fixed clock.
func NewProcessOperationUseCase(uow UnitOfWork, metrics OperationMetrics) *ProcessOperationUseCase {
	return &ProcessOperationUseCase{uow: uow, metrics: metrics, now: time.Now, pendingTTL: 24 * time.Hour}
}

// SetPendingReferenceTTL applies the configured lifetime before an accepted
// out-of-order operation is finally rejected. It is called during Fx graph
// construction, before either HTTP or SQS workers start.
func (uc *ProcessOperationUseCase) SetPendingReferenceTTL(ttl time.Duration) {
	if ttl > 0 {
		uc.pendingTTL = ttl
	}
}

// Prepare runs decision 3's step 1 - validate the idempotency key, compute
// the canonical PayloadHash, and evaluate the request against the domain
// rules that need no database state - entirely outside any transaction. A
// correctable input error (a bad idempotency key, an unhashable payload, a
// forbidden reference, ...) comes back classified here, before ExecuteInTx
// - and the transaction it runs inside - is ever invoked: an entry a
// provider can simply retry with a corrected body must never open and roll
// back a database transaction to say so.
//
// Evaluate is called with no resolved reference, which is final for BET,
// WIN without a reference and LOSS - none of them ever need one. For
// REFUND, ROLLBACK or a WIN that names a reference, Evaluate always answers
// WaitForReference here (it is deliberately given no reference to resolve
// against); ExecuteInTx's resolveDecision reruns Evaluate with the
// reference actually resolved from the database, under the wallet lock,
// once ExecuteInTx has one to offer (decision 3, step 4).
func (uc *ProcessOperationUseCase) Prepare(input ProcessOperationInput) (PreparedOperation, error) {
	if !isValidIdempotencyKey(input.IdempotencyKey) {
		return PreparedOperation{}, operation.ErrMissingIdempotencyKey
	}

	hash, err := operation.PayloadHash(input.Request)
	if err != nil {
		return PreparedOperation{}, err
	}

	decision, err := operation.Evaluate(input.Request, nil, false)
	if err != nil {
		return PreparedOperation{}, err
	}

	return PreparedOperation{input: input, hash: hash, decision: decision, ready: true}, nil
}

// ExecuteInTx runs decision 3's steps 2-6 - lock the wallet, classify the
// attempt against whatever is already committed, INSERT with ON CONFLICT DO
// NOTHING, the version-conditioned wallet UPDATE, the ledger entry and the
// outbox records - using only the repositories it is handed and a
// PreparedOperation that Prepare has already validated and hashed. It never
// repeats that validation or hashing, and it never opens or closes a
// transaction itself: Process (below) binds it to the transaction its own
// UnitOfWork opens for HTTP, and ticket 13's SQS consumer is meant to bind
// it to the very transaction its inbox insert commits in, so a failure
// between the two can never leave the wallet movement committed without the
// inbox record, or the other way around.
//
// A concurrent INSERT collision (step 5's backstop finding nothing to
// insert) is reported as ErrRetryInNewTransaction rather than retried here:
// reclassifying needs the winning writer's commit to already be visible,
// which only a fresh transaction can guarantee.
//
// A PreparedOperation that did not come from Prepare - the zero value,
// above all - is rejected as ErrOperationNotPrepared before any repository
// is touched: no panic, no query, no chance of persisting a hash or
// decision this use case never actually validated.
func (uc *ProcessOperationUseCase) ExecuteInTx(ctx context.Context, repos Repositories, prepared PreparedOperation) (ProcessOperationResult, error) {
	if !prepared.ready {
		return ProcessOperationResult{}, ErrOperationNotPrepared
	}

	now := uc.now()

	result, retry, err := uc.attempt(ctx, repos, prepared.input, prepared.hash, prepared.decision, now)
	if err != nil {
		return ProcessOperationResult{}, err
	}
	if retry {
		return ProcessOperationResult{}, ErrRetryInNewTransaction
	}
	return result, nil
}

// Process is the HTTP-facing entry point: it runs Prepare outside any
// transaction, then opens its own UnitOfWork transaction around ExecuteInTx
// and, on ErrRetryInNewTransaction, rolls back and retries exactly once in a
// brand new transaction (spec, decision 3, step 5: "faz rollback e
// classifica em nova transação"). A correctable Prepare error never reaches
// the UnitOfWork at all. Every corrigible or conflicting outcome (a
// *operation.Error) is returned unpersisted; a nil error always carries a
// terminal ProcessOperationResult.
func (uc *ProcessOperationUseCase) Process(ctx context.Context, input ProcessOperationInput) (ProcessOperationResult, error) {
	started := uc.now()

	prepared, err := uc.Prepare(input)
	if err != nil {
		return ProcessOperationResult{}, err
	}

	result, err := uc.processWithRetry(ctx, prepared)
	if err != nil {
		return ProcessOperationResult{}, err
	}

	uc.RecordOutcome(input.Channel, result, nil, uc.now().Sub(started))
	return result, nil
}

// RecordOutcome records the observability side effects of a completed
// operation. HTTP calls it through Process, while the SQS adapter calls it
// only after committing its inbox transaction, so both transports contribute
// to the same channel-labelled operation, replay, and latency metrics.
func (uc *ProcessOperationUseCase) RecordOutcome(channel string, result ProcessOperationResult, err error, duration time.Duration) {
	if err != nil {
		return
	}
	uc.metrics.ObserveOperation(channel, string(result.kind), string(result.Status), duration)
	if result.IdempotentReplay {
		uc.metrics.ObserveDuplicate(channel)
	}
}

// processWithRetry runs ExecuteInTx inside one fresh transaction and, on a
// concurrent INSERT collision, retries exactly once more in another fresh
// transaction - by then the winning writer has committed, so ExecuteInTx's
// own lookupExisting step finds its row and answers with a replay or a
// conflict instead of trying to insert again. A second collision in a row
// is not retried further: it is reported as a transient failure, which the
// HTTP layer answers with 503 and Retry-After.
func (uc *ProcessOperationUseCase) processWithRetry(ctx context.Context, prepared PreparedOperation) (ProcessOperationResult, error) {
	result, err := uc.runInNewTx(ctx, prepared)
	if !errors.Is(err, ErrRetryInNewTransaction) {
		return result, err
	}

	uc.metrics.ObserveConcurrencyConflict()
	result, err = uc.runInNewTx(ctx, prepared)
	if errors.Is(err, ErrRetryInNewTransaction) {
		return ProcessOperationResult{}, operation.ErrTemporarilyUnavailable
	}
	return result, err
}

func (uc *ProcessOperationUseCase) runInNewTx(ctx context.Context, prepared PreparedOperation) (ProcessOperationResult, error) {
	var result ProcessOperationResult
	err := uc.uow.WithinTx(ctx, func(ctx context.Context, repos Repositories) error {
		r, err := uc.ExecuteInTx(ctx, repos, prepared)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	return result, err
}

// attempt runs one full pass of the concurrency flow's steps 2-6 within a
// single transaction's repositories: lock the wallet, classify the attempt,
// and - for a genuinely new one - process it. retry reports that the
// caller must roll back and reclassify in a fresh transaction (the step-5
// backstop).
func (uc *ProcessOperationUseCase) attempt(ctx context.Context, repos Repositories, input ProcessOperationInput, hash string, decision operation.Decision, now time.Time) (result ProcessOperationResult, retry bool, err error) {
	req := input.Request

	walletValue, err := repos.Wallets.FindForUpdate(ctx, req.WalletID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ProcessOperationResult{}, false, operation.ErrWalletNotFound
		}
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: lock wallet: %w", err)
	}
	if walletValue.PlayerID() != req.PlayerID {
		return ProcessOperationResult{}, false, operation.ErrWalletPlayerMismatch
	}
	requestCurrency, err := req.Money.Currency()
	if err != nil {
		// Evaluate already validated this; a failure here would be a bug,
		// not a provider input problem.
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: request currency: %w", err)
	}
	if walletValue.Currency() != requestCurrency {
		return ProcessOperationResult{}, false, operation.ErrWalletCurrencyMismatch
	}

	if result, handled, err := uc.lookupExisting(ctx, repos, req, input.IdempotencyKey, hash); err != nil || handled {
		return result, false, err
	}

	resolvedDecision, reference, err := uc.resolveDecision(ctx, repos, req, decision)
	if err != nil {
		return ProcessOperationResult{}, false, err
	}
	if resolvedDecision.Action == operation.WaitForReference {
		return uc.processPendingNew(ctx, repos, walletValue, input, hash, now)
	}

	return uc.processNew(ctx, repos, walletValue, input, hash, resolvedDecision, reference, now)
}

func (uc *ProcessOperationUseCase) processPendingNew(ctx context.Context, repos Repositories, walletValue *domainwallet.Wallet, input ProcessOperationInput, hash string, now time.Time) (ProcessOperationResult, bool, error) {
	transactionID, err := newID()
	if err != nil {
		return ProcessOperationResult{}, false, err
	}
	referenceExternalID := ""
	if input.Request.ReferenceExternalTransactionID != nil {
		referenceExternalID = *input.Request.ReferenceExternalTransactionID
	}
	transaction, err := domainwallet.NewExternalTransaction(domainwallet.ExternalTransactionInput{ID: transactionID, ExternalTransactionID: input.Request.ExternalTransactionID, ProviderID: input.Request.ProviderID, IdempotencyKey: input.IdempotencyKey, PayloadHash: hash, WalletID: input.Request.WalletID, PlayerID: input.Request.PlayerID, RoundID: input.Request.RoundID, GameID: input.Request.GameID, Kind: input.Request.Kind, Money: input.Request.Money, ReferenceExternalTransactionID: referenceExternalID, CreatedAt: now})
	if err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: build pending wager transaction: %w", err)
	}
	if err := transaction.MarkPendingReference(now); err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: mark pending reference: %w", err)
	}
	expiresAt, inserted, err := repos.Transactions.InsertPending(ctx, transaction, now, uc.pendingTTL)
	if err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: insert pending wager transaction: %w", err)
	}
	if !inserted {
		return ProcessOperationResult{}, true, nil
	}
	eventID, err := newID()
	if err != nil {
		return ProcessOperationResult{}, false, err
	}
	event, err := domainwallet.NewWagerTransactionPendingReference(domainwallet.EventMetadata{EventID: eventID, CorrelationID: input.CorrelationID, CausationID: input.CausationID, OccurredAt: now}, transaction, expiresAt)
	if err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: build pending reference event: %w", err)
	}
	if err := repos.Outbox.Insert(ctx, OutboxRecord{EventID: event.EventID, EventType: event.EventType, AggregateType: "WagerTransaction", AggregateID: event.AggregateID, EventVersion: event.Version, OccurredAt: now, Payload: event}); err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: insert pending reference event: %w", err)
	}
	return ProcessOperationResult{TransactionID: transaction.ID(), Status: domainwallet.PendingReference, PendingExpiresAt: &expiresAt, kind: input.Request.Kind}, false, nil
}

// ResumePending retries one previously accepted operation in its own
// transaction. It locks wallet first and transaction second, matching the
// synchronous flow; a worker that lost its lease therefore rereads the state
// under the same ordering before it can create a financial effect.
func (uc *ProcessOperationUseCase) ResumePending(ctx context.Context, transactionID, walletID string, settings PendingResumeSettings) (PendingResumeOutcome, error) {
	var outcome PendingResumeOutcome
	err := uc.uow.WithinTx(ctx, func(ctx context.Context, repos Repositories) error {
		now := uc.now()
		walletValue, err := repos.Wallets.FindForUpdate(ctx, walletID)
		if err != nil {
			return fmt.Errorf("walletapp: lock pending wallet: %w", err)
		}
		pending, err := repos.Transactions.FindPendingForUpdate(ctx, transactionID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("walletapp: lock pending transaction: %w", err)
		}
		transaction := pending.Transaction
		if transaction.Status() != domainwallet.PendingReference {
			return nil
		}

		req := operation.Request{ProviderID: transaction.ProviderID(), ExternalTransactionID: transaction.ExternalTransactionID(), PlayerID: transaction.PlayerID(), WalletID: transaction.WalletID(), RoundID: transaction.RoundID(), GameID: transaction.GameID(), Kind: transaction.Kind(), Money: transaction.Money()}
		refExternalID := transaction.ReferenceExternalTransactionID()
		req.ReferenceExternalTransactionID = &refExternalID
		decision, err := operation.Evaluate(req, nil, false)
		if err != nil {
			return fmt.Errorf("walletapp: evaluate pending transaction: %w", err)
		}
		decision, reference, err := uc.resolveDecision(ctx, repos, req, decision)
		if err != nil {
			return err
		}
		if decision.Action == operation.Process || decision.Action == operation.Reject {
			if decision.Action == operation.Reject {
				if reference != nil {
					if err := transaction.ResolveReference(reference.ID()); err != nil {
						return fmt.Errorf("walletapp: resolve rejected pending reference: %w", err)
					}
				}
				return uc.rejectPending(ctx, repos, transaction, walletValue, decision.Error, now, &outcome)
			}
			if reference == nil {
				return errors.New("walletapp: process pending transaction without resolved reference")
			}
			if err := transaction.ResolveReference(reference.ID()); err != nil {
				return fmt.Errorf("walletapp: resolve pending reference: %w", err)
			}
			return uc.processPending(ctx, repos, transaction, walletValue, decision, now, &outcome)
		}

		if pending.Expired || (settings.MaxAttempts > 0 && pending.Attempts >= settings.MaxAttempts) {
			code := operation.ErrReferenceNotFound
			reference, findErr := repos.Transactions.FindReference(ctx, transaction.ProviderID(), refExternalID)
			if findErr == nil && reference != nil {
				code = operation.ErrReferenceNotProcessed
				if err := transaction.ResolveReference(reference.ID()); err != nil {
					return fmt.Errorf("walletapp: resolve expired pending reference: %w", err)
				}
			} else if findErr != nil && !errors.Is(findErr, ErrNotFound) {
				return fmt.Errorf("walletapp: find expired pending reference: %w", findErr)
			}
			return uc.rejectPending(ctx, repos, transaction, walletValue, code, now, &outcome)
		}
		if decision.Action == operation.WaitForReference {
			if err := repos.Transactions.ReschedulePending(ctx, transaction.ID(), pending.Attempts+1, settings.RetryDelay); err != nil {
				return fmt.Errorf("walletapp: reschedule pending transaction: %w", err)
			}
			outcome = PendingRescheduled
			return nil
		}
		return errors.New("walletapp: unexpected pending reference decision")
	})
	return outcome, err
}

// FailPending records an auditable terminal outcome after the worker has
// classified a resume error as permanent. It opens a fresh transaction because
// the transaction that discovered the failure was rolled back.
func (uc *ProcessOperationUseCase) FailPending(ctx context.Context, transactionID, walletID string) error {
	return uc.uow.WithinTx(ctx, func(ctx context.Context, repos Repositories) error {
		now := uc.now()
		_, err := repos.Wallets.FindForUpdate(ctx, walletID)
		if err != nil {
			return fmt.Errorf("walletapp: lock failed pending wallet: %w", err)
		}
		pending, err := repos.Transactions.FindPendingForUpdate(ctx, transactionID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("walletapp: lock failed pending transaction: %w", err)
		}
		transaction := pending.Transaction
		if transaction.Status() != domainwallet.PendingReference {
			return nil
		}
		if err := transaction.MarkFailed(string(operation.CodePermanentProcessingFailure), now); err != nil {
			return fmt.Errorf("walletapp: fail pending transaction: %w", err)
		}
		if err := repos.Transactions.CompletePending(ctx, transaction, nil); err != nil {
			return fmt.Errorf("walletapp: persist failed pending transaction: %w", err)
		}
		return nil
	})
}

// ReschedulePendingAfterFailure gives a transient worker failure a fresh,
// short transaction in which to persist its retry. The original transaction
// was rolled back, so its retry metadata could not be retained there.
func (uc *ProcessOperationUseCase) ReschedulePendingAfterFailure(ctx context.Context, transactionID, walletID string, delay time.Duration) error {
	return uc.uow.WithinTx(ctx, func(ctx context.Context, repos Repositories) error {
		if _, err := repos.Wallets.FindForUpdate(ctx, walletID); err != nil {
			return fmt.Errorf("walletapp: lock retry pending wallet: %w", err)
		}
		pending, err := repos.Transactions.FindPendingForUpdate(ctx, transactionID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("walletapp: lock retry pending transaction: %w", err)
		}
		if pending.Transaction.Status() != domainwallet.PendingReference {
			return nil
		}
		if err := repos.Transactions.ReschedulePending(ctx, transactionID, pending.Attempts+1, delay); err != nil {
			return fmt.Errorf("walletapp: reschedule transient pending failure: %w", err)
		}
		return nil
	})
}

func (uc *ProcessOperationUseCase) rejectPending(ctx context.Context, repos Repositories, transaction *domainwallet.WagerTransaction, walletValue *domainwallet.Wallet, rejection *operation.Error, now time.Time, outcome *PendingResumeOutcome) error {
	if err := transaction.MarkRejected(string(rejection.Code()), now); err != nil {
		return fmt.Errorf("walletapp: reject pending transaction: %w", err)
	}
	balance, err := walletValue.Balance().MinorUnits()
	if err != nil {
		return fmt.Errorf("walletapp: pending rejection balance: %w", err)
	}
	if err := repos.Transactions.CompletePending(ctx, transaction, &balance); err != nil {
		return fmt.Errorf("walletapp: persist pending rejection: %w", err)
	}
	if err := uc.writeOutboxEvents(ctx, repos, transaction, walletValue, nil, transaction.ID(), transaction.ID(), now); err != nil {
		return err
	}
	*outcome = PendingRejected
	return nil
}

func (uc *ProcessOperationUseCase) processPending(ctx context.Context, repos Repositories, transaction *domainwallet.WagerTransaction, walletValue *domainwallet.Wallet, decision operation.Decision, now time.Time, outcome *PendingResumeOutcome) error {
	ledgerEntryID, err := newID()
	if err != nil {
		return err
	}
	movement := domainwallet.MovementInput{LedgerEntryID: ledgerEntryID, TransactionID: transaction.ID(), Money: transaction.Money(), OccurredAt: now}
	var entry *domainwallet.WalletLedgerEntry
	if decision.Direction == domainwallet.Debit {
		entry, err = walletValue.Debit(movement)
	} else {
		entry, err = walletValue.Credit(movement)
	}
	if err != nil {
		if errors.Is(err, domainwallet.ErrInsufficientFunds) {
			return uc.rejectPending(ctx, repos, transaction, walletValue, operation.InsufficientFundsFor(transaction.Kind()), now, outcome)
		}
		return fmt.Errorf("walletapp: apply pending movement: %w", err)
	}
	if err := transaction.MarkProcessed(now); err != nil {
		return fmt.Errorf("walletapp: process pending transaction: %w", err)
	}
	balance, err := walletValue.Balance().MinorUnits()
	if err != nil {
		return fmt.Errorf("walletapp: pending result balance: %w", err)
	}
	if err := repos.Transactions.CompletePending(ctx, transaction, &balance); err != nil {
		return fmt.Errorf("walletapp: persist pending completion: %w", err)
	}
	if err := repos.Ledger.Insert(ctx, entry); err != nil {
		return fmt.Errorf("walletapp: insert pending ledger entry: %w", err)
	}
	if err := repos.Wallets.UpdateBalance(ctx, walletValue, walletValue.Version()-1); err != nil {
		return fmt.Errorf("walletapp: update pending wallet: %w", err)
	}
	if err := uc.writeOutboxEvents(ctx, repos, transaction, walletValue, entry, transaction.ID(), transaction.ID(), now); err != nil {
		return err
	}
	*outcome = PendingProcessed
	return nil
}

// resolveDecision finalizes prepared's preliminary decision for a genuinely
// new attempt. BET, WIN without a reference and LOSS already carry their
// final Decision from Prepare - Action is Process and there is nothing to
// resolve. REFUND, ROLLBACK and a WIN naming a reference always come back
// from Prepare as WaitForReference, since Prepare's own Evaluate call was
// deliberately given no reference; this looks the reference up by
// (providerId, referenceExternalTransactionId), scoped to the same
// provider (spec: "a referência é resolvida por (providerId,
// referenceExternalTransactionId)"), and - once it is PROCESSED - whether
// it already carries a successful reversal, then reruns Evaluate for the
// definitive answer (spec, decision 3, step 4: "resolve a referência ... e
// aplica as regras de domínio"). The reference is visible here, not raced,
// because every write that could produce or change it also has to hold
// this same wallet's FOR UPDATE lock first - REFUND, ROLLBACK and a
// referenced WIN all require reference.WalletID() == req.WalletID, so they
// can never disagree about which wallet's lock protects them.
func (uc *ProcessOperationUseCase) resolveDecision(ctx context.Context, repos Repositories, req operation.Request, decision operation.Decision) (operation.Decision, *domainwallet.WagerTransaction, error) {
	if decision.Action != operation.WaitForReference {
		return decision, nil, nil
	}

	reference, err := repos.Transactions.FindReference(ctx, req.ProviderID, *req.ReferenceExternalTransactionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return decision, nil, nil
		}
		return operation.Decision{}, nil, fmt.Errorf("walletapp: find reference: %w", err)
	}

	var alreadyReversed bool
	if reference.Status() == domainwallet.Processed {
		alreadyReversed, err = repos.Transactions.ExistsSuccessfulReversal(ctx, reference.ID())
		if err != nil {
			return operation.Decision{}, nil, fmt.Errorf("walletapp: check reference reversed: %w", err)
		}
	}

	resolved, err := operation.Evaluate(req, reference, alreadyReversed)
	if err != nil {
		return operation.Decision{}, nil, err
	}
	return resolved, reference, nil
}

// lookupExisting classifies the attempt against whatever the same provider
// has already committed (spec, decision 3, step 3). handled is true when the
// classification alone answers the request - a replay or a conflict - and
// false for a genuinely new attempt the caller should go on to process.
func (uc *ProcessOperationUseCase) lookupExisting(ctx context.Context, repos Repositories, req operation.Request, idempotencyKey, hash string) (result ProcessOperationResult, handled bool, err error) {
	byKey, err := findExisting(ctx, repos.Transactions.FindByIdempotencyKey, req.ProviderID, idempotencyKey)
	if err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: find by idempotency key: %w", err)
	}
	byExternal, err := findExisting(ctx, repos.Transactions.FindByExternalTransactionID, req.ProviderID, req.ExternalTransactionID)
	if err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: find by external transaction id: %w", err)
	}

	classification := operation.ClassifyAttempt(
		operation.Attempt{IdempotencyKey: idempotencyKey, PayloadHash: hash, ExternalTransactionID: req.ExternalTransactionID},
		toAttempt(byKey), toAttempt(byExternal),
	)
	switch classification.Kind {
	case operation.Replay:
		result, err := replayResult(byKey)
		result.kind = req.Kind
		return result, true, err
	case operation.AttemptConflict:
		return ProcessOperationResult{}, true, classification.Error
	default:
		return ProcessOperationResult{}, false, nil
	}
}

func findExisting(ctx context.Context, find func(context.Context, string, string) (*ExistingTransaction, error), providerID, key string) (*ExistingTransaction, error) {
	record, err := find(ctx, providerID, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return record, nil
}

func toAttempt(record *ExistingTransaction) *operation.Attempt {
	if record == nil {
		return nil
	}
	return &operation.Attempt{IdempotencyKey: record.IdempotencyKey, PayloadHash: record.PayloadHash, ExternalTransactionID: record.ExternalTransactionID}
}

// replayResult rebuilds the exact answer the original processing computed,
// so a replay never drifts from it even if the wallet has moved since
// (spec: "a replay devolve o saldo daquele momento"). A persisted balance
// that can no longer be rebuilt into a valid Money is a corrupted record,
// not something to answer with a silently-zeroed balance.
func replayResult(record *ExistingTransaction) (ProcessOperationResult, error) {
	result := ProcessOperationResult{TransactionID: record.TransactionID, Status: record.Status, FailureCode: record.FailureCode, PendingExpiresAt: record.PendingExpiresAt, IdempotentReplay: true}
	if record.ResultingBalance == nil {
		return result, nil
	}
	balance, err := money.New(*record.ResultingBalance, record.Currency)
	if err != nil {
		return ProcessOperationResult{}, fmt.Errorf("%w: %v", ErrCorruptedResultingBalance, err)
	}
	result.Balance = balance
	return result, nil
}

// processNew builds and persists a genuinely new attempt: the transaction
// row, its movement (if any), the ledger entry and wallet update it produces,
// and the outbox events for the conclusion. reference is the reference
// resolveDecision resolved, nil for BET, LOSS and a WIN with none - its id,
// when present, is what gets persisted as the transaction's own resolved
// referenceTransactionID (spec: "a referência resolvida fica persistida"),
// whether decision ultimately processes or rejects the operation.
func (uc *ProcessOperationUseCase) processNew(ctx context.Context, repos Repositories, walletValue *domainwallet.Wallet, input ProcessOperationInput, hash string, decision operation.Decision, reference *domainwallet.WagerTransaction, now time.Time) (ProcessOperationResult, bool, error) {
	req := input.Request

	transactionID, err := newID()
	if err != nil {
		return ProcessOperationResult{}, false, err
	}

	referenceExternalID := ""
	if req.ReferenceExternalTransactionID != nil {
		referenceExternalID = *req.ReferenceExternalTransactionID
	}
	referenceTransactionID := ""
	if reference != nil {
		referenceTransactionID = reference.ID()
	}

	transaction, err := domainwallet.NewExternalTransaction(domainwallet.ExternalTransactionInput{
		ID: transactionID, ExternalTransactionID: req.ExternalTransactionID, ProviderID: req.ProviderID,
		IdempotencyKey: input.IdempotencyKey, PayloadHash: hash, WalletID: req.WalletID, PlayerID: req.PlayerID,
		RoundID: req.RoundID, GameID: req.GameID, Kind: req.Kind, Money: req.Money,
		ReferenceExternalTransactionID: referenceExternalID, ReferenceTransactionID: referenceTransactionID, CreatedAt: now,
	})
	if err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: build wager transaction: %w", err)
	}

	var (
		entry         *domainwallet.WalletLedgerEntry
		rejectionCode *operation.Error
	)
	switch {
	// A reference that resolved to a durable rejection (REFERENCE_NOT_
	// PROCESSED, REFERENCE_ALREADY_REVERSED, REFERENCE_MISMATCH,
	// REFERENCE_AMOUNT_MISMATCH, REFERENCE_KIND_NOT_REVERSIBLE) never
	// attempts a movement: decision.Error already carries the exact code
	// operation.Evaluate classified it under.
	case decision.Action == operation.Reject:
		rejectionCode = decision.Error
	// LOSS carries an empty Direction and never calls Debit or Credit, so
	// its version never changes (spec: "LOSS não chama débito nem crédito,
	// então a versão não muda").
	case decision.Direction != "":
		ledgerEntryID, err := newID()
		if err != nil {
			return ProcessOperationResult{}, false, err
		}
		movementInput := domainwallet.MovementInput{LedgerEntryID: ledgerEntryID, TransactionID: transactionID, Money: req.Money, OccurredAt: now}
		if decision.Direction == domainwallet.Debit {
			entry, err = walletValue.Debit(movementInput)
		} else {
			entry, err = walletValue.Credit(movementInput)
		}
		if err != nil {
			if !errors.Is(err, domainwallet.ErrInsufficientFunds) {
				return ProcessOperationResult{}, false, fmt.Errorf("walletapp: apply movement: %w", err)
			}
			// A BET without enough balance, or a reversal that would leave
			// the balance negative, is a durable, auditable rejection, not a
			// transient failure: it is still persisted below, with wallet
			// and version left untouched (spec: "aposta sem saldo é
			// rejeitada como REJECTED com INSUFFICIENT_FUNDS"; "reversão com
			// débito acima do saldo devolve REVERSAL_INSUFFICIENT_FUNDS").
			rejectionCode = operation.InsufficientFundsFor(req.Kind)
		}
	}

	if rejectionCode != nil {
		if err := transaction.MarkRejected(string(rejectionCode.Code()), now); err != nil {
			return ProcessOperationResult{}, false, fmt.Errorf("walletapp: mark rejected: %w", err)
		}
	} else if err := transaction.MarkProcessed(now); err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: mark processed: %w", err)
	}

	resultingBalance, err := walletValue.Balance().MinorUnits()
	if err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: resulting balance: %w", err)
	}

	inserted, err := repos.Transactions.InsertNew(ctx, transaction, &resultingBalance)
	if err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("walletapp: insert wager transaction: %w", err)
	}
	if !inserted {
		return ProcessOperationResult{}, true, nil
	}

	if entry != nil {
		if err := repos.Ledger.Insert(ctx, entry); err != nil {
			return ProcessOperationResult{}, false, fmt.Errorf("walletapp: insert ledger entry: %w", err)
		}
		if err := repos.Wallets.UpdateBalance(ctx, walletValue, walletValue.Version()-1); err != nil {
			if errors.Is(err, ErrConcurrencyConflict) {
				uc.metrics.ObserveConcurrencyConflict()
			}
			return ProcessOperationResult{}, false, fmt.Errorf("walletapp: update wallet balance: %w", err)
		}
	}

	if err := uc.writeOutboxEvents(ctx, repos, transaction, walletValue, entry, input.CorrelationID, input.CausationID, now); err != nil {
		return ProcessOperationResult{}, false, err
	}

	balance, err := money.New(resultingBalance, walletValue.Currency())
	if err != nil {
		return ProcessOperationResult{}, false, fmt.Errorf("%w: %v", ErrCorruptedResultingBalance, err)
	}
	result := ProcessOperationResult{TransactionID: transactionID, Status: transaction.Status(), FailureCode: transaction.FailureCode(), Balance: balance, kind: req.Kind}
	return result, false, nil
}

// writeOutboxEvents records WagerTransactionProcessed for every conclusion
// (including LOSS) or WagerTransactionRejected for a durable rejection, plus
// WalletBalanceChanged only when a movement actually changed the balance
// (spec: "WagerTransactionProcessed em toda conclusão, WalletBalanceChanged
// só com mudança de saldo").
func (uc *ProcessOperationUseCase) writeOutboxEvents(ctx context.Context, repos Repositories, transaction *domainwallet.WagerTransaction, walletValue *domainwallet.Wallet, entry *domainwallet.WalletLedgerEntry, correlationID, causationID string, now time.Time) error {
	eventID, err := newID()
	if err != nil {
		return err
	}
	metadata := domainwallet.EventMetadata{EventID: eventID, CorrelationID: correlationID, CausationID: causationID, OccurredAt: now}

	if transaction.Status() == domainwallet.Rejected {
		event, err := domainwallet.NewWagerTransactionRejected(metadata, transaction)
		if err != nil {
			return fmt.Errorf("walletapp: build rejected event: %w", err)
		}
		if err := repos.Outbox.Insert(ctx, OutboxRecord{
			EventID: event.EventID, EventType: event.EventType, AggregateType: "WagerTransaction",
			AggregateID: event.AggregateID, EventVersion: event.Version, OccurredAt: now, Payload: event,
		}); err != nil {
			return fmt.Errorf("walletapp: insert rejected event: %w", err)
		}
		return nil
	}
	if transaction.Status() == domainwallet.Failed {
		return nil
	}

	processedEvent, err := domainwallet.NewWagerTransactionProcessed(metadata, transaction)
	if err != nil {
		return fmt.Errorf("walletapp: build processed event: %w", err)
	}
	if err := repos.Outbox.Insert(ctx, OutboxRecord{
		EventID: processedEvent.EventID, EventType: processedEvent.EventType, AggregateType: "WagerTransaction",
		AggregateID: processedEvent.AggregateID, EventVersion: processedEvent.Version, OccurredAt: now, Payload: processedEvent,
	}); err != nil {
		return fmt.Errorf("walletapp: insert processed event: %w", err)
	}

	if entry == nil {
		return nil
	}

	balanceEventID, err := newID()
	if err != nil {
		return err
	}
	balanceEvent, err := domainwallet.NewWalletBalanceChanged(domainwallet.EventMetadata{EventID: balanceEventID, CorrelationID: correlationID, CausationID: causationID, OccurredAt: now}, walletValue, entry)
	if err != nil {
		return fmt.Errorf("walletapp: build balance event: %w", err)
	}
	if err := repos.Outbox.Insert(ctx, OutboxRecord{
		EventID: balanceEvent.EventID, EventType: balanceEvent.EventType, AggregateType: "Wallet",
		AggregateID: balanceEvent.AggregateID, EventVersion: balanceEvent.Version, OccurredAt: now, Payload: balanceEvent,
	}); err != nil {
		return fmt.Errorf("walletapp: insert balance event: %w", err)
	}
	return nil
}

// isValidIdempotencyKey enforces the spec's format: "um texto ASCII
// imprimível de 1 a 255 caracteres" - printable ASCII is 0x20 (space)
// through 0x7e ('~').
func isValidIdempotencyKey(key string) bool {
	if len(key) == 0 || len(key) > 255 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] > 0x7e {
			return false
		}
	}
	return true
}
