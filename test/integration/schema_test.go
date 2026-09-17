//go:build integration

// This file proves every financial invariant holds at the database level
// with the migrations applied, enforced against wallet_app - the same
// least-privilege role the running service connects as - not against the
// migration owner. A few scenarios (see the "TriggerBlocks...EvenForOwner"
// tests) deliberately use the owner role instead, to prove a trigger holds
// independently of grants, not because of them.
package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// --- wallets ---

func TestWallet_NegativeBalanceFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)

	err := insertWallet(t, ctx, conn, walletRow{
		id: newUUID(t), playerID: newUUID(t), currency: testCurrency,
		balance: -1, version: 1,
	})
	requireErrorCode(t, err, sqlstateCheckViolation, "wallet with negative balance")
}

func TestWallet_InvalidVersionFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)

	err := insertWallet(t, ctx, conn, walletRow{
		id: newUUID(t), playerID: newUUID(t), currency: testCurrency,
		balance: 0, version: 0,
	})
	requireErrorCode(t, err, sqlstateCheckViolation, "wallet with version below 1")
}

func TestWallet_DuplicatePlayerCurrencyFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	playerID := newUUID(t)

	requireNoError(t, insertWallet(t, ctx, conn, walletRow{
		id: newUUID(t), playerID: playerID, currency: testCurrency, balance: 0, version: 1,
	}), "first wallet for player and currency")

	err := insertWallet(t, ctx, conn, walletRow{
		id: newUUID(t), playerID: playerID, currency: testCurrency, balance: 0, version: 1,
	})
	requireErrorCode(t, err, sqlstateUniqueViolation, "second wallet for the same player and currency")
}

// --- wager_transactions ---

func TestWagerTransactions_DuplicateOpeningFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 0)

	first := newOpeningTx(t, w, 10000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, first), "first OPENING for the wallet")

	second := newOpeningTx(t, w, 10000, "PROCESSED")
	err := insertWagerTransaction(t, ctx, conn, second)
	requireErrorCode(t, err, sqlstateUniqueViolation, "second OPENING for the same wallet")
}

func TestWagerTransactions_InternalWithExternalColumnsFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 0)

	row := newOpeningTx(t, w, 10000, "PROCESSED")
	row.providerID = strPtr(sharedTestProviderID) // an INTERNAL/OPENING row must never carry this
	err := insertWagerTransaction(t, ctx, conn, row)
	requireErrorCode(t, err, sqlstateCheckViolation, "INTERNAL transaction carrying an external column")
}

func TestWagerTransactions_ExternalMissingColumnsFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)

	row := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	row.providerID = nil // an EXTERNAL row must always carry its provider
	err := insertWagerTransaction(t, ctx, conn, row)
	requireErrorCode(t, err, sqlstateCheckViolation, "EXTERNAL transaction missing a required external column")
}

func TestWagerTransactions_ReversalWithoutReferenceFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)

	row := newExternalTx(t, w, "REFUND", 5000, "PROCESSED")
	row.referenceExternalID = nil // REFUND without a reference must be rejected
	err := insertWagerTransaction(t, ctx, conn, row)
	requireErrorCode(t, err, sqlstateCheckViolation, "REFUND without a reference")
}

func TestWagerTransactions_FailureCodeWithoutRejectionFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)

	row := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	row.failureCode = strPtr("INSUFFICIENT_FUNDS")
	err := insertWagerTransaction(t, ctx, conn, row)
	requireErrorCode(t, err, sqlstateCheckViolation, "PROCESSED transaction carrying a failure code")
}

func TestWagerTransactions_RejectedWithoutFailureCodeFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)

	row := newExternalTx(t, w, "BET", 5000, "REJECTED")
	// failureCode left nil on purpose.
	err := insertWagerTransaction(t, ctx, conn, row)
	requireErrorCode(t, err, sqlstateCheckViolation, "REJECTED transaction without a failure code")
}

func TestWagerTransactions_DuplicateIdempotencyKeyFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)

	first := newExternalTx(t, w, "BET", 1000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, first), "first transaction")

	second := newExternalTx(t, w, "BET", 1000, "PROCESSED")
	second.idempotencyKey = first.idempotencyKey
	err := insertWagerTransaction(t, ctx, conn, second)
	requireErrorCode(t, err, sqlstateUniqueViolation, "duplicate (providerId, idempotencyKey)")
}

func TestWagerTransactions_DuplicateExternalTransactionIDFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)

	first := newExternalTx(t, w, "BET", 1000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, first), "first transaction")

	second := newExternalTx(t, w, "BET", 1000, "PROCESSED")
	second.externalTransactionID = first.externalTransactionID
	err := insertWagerTransaction(t, ctx, conn, second)
	requireErrorCode(t, err, sqlstateUniqueViolation, "duplicate (providerId, externalTransactionId)")
}

func TestWagerTransactions_UnknownWalletFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)

	ghostWallet := walletRow{id: newUUID(t), playerID: newUUID(t), currency: testCurrency}
	row := newExternalTx(t, ghostWallet, "BET", 1000, "PROCESSED")
	err := insertWagerTransaction(t, ctx, conn, row)
	requireErrorCode(t, err, sqlstateForeignKeyViolation, "wager transaction against a wallet that does not exist")
}

func TestWagerTransactions_TerminalRowUpdateFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)
	tx := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, tx), "fixture: insert PROCESSED bet")

	_, err := conn.Exec(ctx, `UPDATE wager_transactions SET status = 'REJECTED', failure_code = 'PERMANENT_PROCESSING_FAILURE' WHERE id = $1`, tx.id)
	requireErrorCode(t, err, sqlstateRaiseException, "UPDATE of a PROCESSED (terminal) wager transaction")
}

func TestWagerTransactions_PendingReferenceRowUpdateSucceeds(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)
	tx := newExternalTx(t, w, "REFUND", 5000, "PENDING_REFERENCE")
	tx.referenceExternalID = strPtr(newID(t, "ref"))
	requireNoError(t, insertWagerTransaction(t, ctx, conn, tx), "fixture: insert PENDING_REFERENCE refund")

	// A non-terminal row must still be free to resolve into a terminal one -
	// this is the worker of pending references' whole job. resulting_balance
	// has to move from NULL to a concrete value in the same UPDATE, since
	// REJECTED requires one.
	_, err := conn.Exec(ctx, `UPDATE wager_transactions SET status = 'REJECTED', failure_code = 'REFERENCE_NOT_FOUND', resulting_balance = 10000 WHERE id = $1`, tx.id)
	requireNoError(t, err, "resolving a PENDING_REFERENCE row into a terminal status")
}

func TestWagerTransactions_SecondSuccessfulReversalFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)

	bet := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, bet), "fixture: insert bet")

	firstRefund := newExternalTx(t, w, "REFUND", 5000, "PROCESSED")
	firstRefund.referenceExternalID = bet.externalTransactionID
	firstRefund.referenceTransactionID = &bet.id
	requireNoError(t, insertWagerTransaction(t, ctx, conn, firstRefund), "fixture: insert first successful reversal")

	secondRollback := newExternalTx(t, w, "ROLLBACK", 5000, "PROCESSED")
	secondRollback.referenceExternalID = bet.externalTransactionID
	secondRollback.referenceTransactionID = &bet.id
	err := insertWagerTransaction(t, ctx, conn, secondRollback)
	requireErrorCode(t, err, sqlstateUniqueViolation, "second successful reversal (REFUND or ROLLBACK) of the same reference")
}

func TestWagerTransactions_RejectedSecondReversalSucceeds(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)

	bet := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, bet), "fixture: insert bet")

	firstRefund := newExternalTx(t, w, "REFUND", 5000, "PROCESSED")
	firstRefund.referenceExternalID = bet.externalTransactionID
	firstRefund.referenceTransactionID = &bet.id
	requireNoError(t, insertWagerTransaction(t, ctx, conn, firstRefund), "fixture: insert first successful reversal")

	secondRollback := newExternalTx(t, w, "ROLLBACK", 5000, "REJECTED")
	secondRollback.referenceExternalID = bet.externalTransactionID
	secondRollback.referenceTransactionID = &bet.id
	secondRollback.failureCode = strPtr("REFERENCE_ALREADY_REVERSED")
	err := insertWagerTransaction(t, ctx, conn, secondRollback)
	requireNoError(t, err, "a second, REJECTED reversal for the same reference must be allowed")
}

// TestWagerTransactions_PendingStatusFails proves PENDING - the in-memory
// only state before a transaction's first INSERT - can never be persisted.
// The only non-terminal status a raw INSERT may create is PENDING_REFERENCE;
// nothing selects a stray PENDING row, so it would sit forever unresolved.
func TestWagerTransactions_PendingStatusFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)

	row := newExternalTx(t, w, "BET", 5000, "PENDING")
	err := insertWagerTransaction(t, ctx, conn, row)
	requireErrorCode(t, err, sqlstateCheckViolation, "PENDING is not a persistable status")
}

// TestWagerTransactions_ResultingBalanceTerminalCheck covers every branch
// of wager_transactions_resulting_balance_terminal_check: PROCESSED and
// REJECTED require a non-negative resulting_balance, PENDING_REFERENCE and
// FAILED forbid one.
func TestWagerTransactions_ResultingBalanceTerminalCheck(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name             string
		status           string
		failureCode      *string
		resultingBalance *int64
		wantErr          bool
	}{
		{"PROCESSED with resulting balance succeeds", "PROCESSED", nil, int64Ptr(5000), false},
		{"PROCESSED without resulting balance fails", "PROCESSED", nil, nil, true},
		{"PROCESSED with negative resulting balance fails", "PROCESSED", nil, int64Ptr(-1), true},
		{"REJECTED with resulting balance succeeds", "REJECTED", strPtr("INSUFFICIENT_FUNDS"), int64Ptr(10000), false},
		{"REJECTED without resulting balance fails", "REJECTED", strPtr("INSUFFICIENT_FUNDS"), nil, true},
		{"PENDING_REFERENCE without resulting balance succeeds", "PENDING_REFERENCE", nil, nil, false},
		{"PENDING_REFERENCE with resulting balance fails", "PENDING_REFERENCE", nil, int64Ptr(0), true},
		{"FAILED without resulting balance succeeds", "FAILED", strPtr("PERMANENT_PROCESSING_FAILURE"), nil, false},
		{"FAILED with resulting balance fails", "FAILED", strPtr("PERMANENT_PROCESSING_FAILURE"), int64Ptr(0), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := connectApp(t, ctx)
			w := newWallet(t, ctx, conn, 10000)
			row := newExternalTx(t, w, "BET", 5000, tc.status)
			row.failureCode = tc.failureCode
			row.resultingBalance = tc.resultingBalance

			err := insertWagerTransaction(t, ctx, conn, row)
			if tc.wantErr {
				requireErrorCode(t, err, sqlstateCheckViolation, tc.name)
			} else {
				requireNoError(t, err, tc.name)
				settleIfPendingReference(t, ctx, conn, row)
			}
		})
	}
}

// settleIfPendingReference retires a fixture row left in
// PENDING_REFERENCE, the one non-terminal status a direct INSERT can
// create. Such a row is shaped to exercise a CHECK constraint, not to be
// processable: a BET, for one, never carries a reference, so the
// pending-reference worker of any instance later running against this same
// database claims it, cannot resolve it, classifies that as transient and
// reschedules it forever. The multi-instance suite, which shares this
// Postgres, then waits on a backlog gauge that can never reach zero.
// Settling the row here leaves this test's own subject - the constraint -
// untouched and leaves nothing durable behind.
func settleIfPendingReference(t *testing.T, ctx context.Context, conn *pgx.Conn, row wagerTxRow) {
	t.Helper()
	if row.status != "PENDING_REFERENCE" {
		return
	}
	_, err := conn.Exec(ctx, `UPDATE wager_transactions SET status = 'REJECTED', failure_code = 'REFERENCE_NOT_FOUND', resulting_balance = 0 WHERE id = $1`, row.id)
	requireNoError(t, err, "settling a PENDING_REFERENCE fixture row")
}

// TestWagerTransactions_AmountByKindCheck covers
// wager_transactions_amount_by_kind_check for the five external kinds:
// BET, WIN, REFUND and ROLLBACK must be strictly positive, LOSS must be
// exactly zero. OPENING is covered separately in
// TestWagerTransactions_OpeningAmountByKindCheck, since it is the only
// kind that is INTERNAL rather than EXTERNAL.
func TestWagerTransactions_AmountByKindCheck(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		kind    string
		amount  int64
		wantErr bool
	}{
		{"BET with zero amount fails", "BET", 0, true},
		{"BET with positive amount succeeds", "BET", 1, false},
		{"WIN with zero amount fails", "WIN", 0, true},
		{"WIN with positive amount succeeds", "WIN", 1, false},
		{"REFUND with zero amount fails", "REFUND", 0, true},
		{"REFUND with positive amount succeeds", "REFUND", 1, false},
		{"ROLLBACK with zero amount fails", "ROLLBACK", 0, true},
		{"ROLLBACK with positive amount succeeds", "ROLLBACK", 1, false},
		{"LOSS with zero amount succeeds", "LOSS", 0, false},
		{"LOSS with positive amount fails", "LOSS", 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := connectApp(t, ctx)
			w := newWallet(t, ctx, conn, 10000)
			row := newExternalTx(t, w, tc.kind, tc.amount, "PROCESSED")

			if tc.kind == "REFUND" || tc.kind == "ROLLBACK" {
				bet := newExternalTx(t, w, "BET", 5000, "PROCESSED")
				requireNoError(t, insertWagerTransaction(t, ctx, conn, bet), "fixture: insert bet to reverse")
				row.referenceExternalID = bet.externalTransactionID
				row.referenceTransactionID = &bet.id
			}

			err := insertWagerTransaction(t, ctx, conn, row)
			if tc.wantErr {
				requireErrorCode(t, err, sqlstateCheckViolation, tc.name)
			} else {
				requireNoError(t, err, tc.name)
			}
		})
	}
}

// TestWagerTransactions_OpeningAmountByKindCheck proves OPENING follows the
// same amount-by-kind policy as every other credit: strictly positive. That a
// zero initial balance never creates an OPENING row at all is a domain-level
// choice; this constraint is the schema's defense in depth against a raw SQL
// OPENING of zero.
func TestWagerTransactions_OpeningAmountByKindCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("OPENING with zero amount fails", func(t *testing.T) {
		conn := connectApp(t, ctx)
		w := newWallet(t, ctx, conn, 0)
		row := newOpeningTx(t, w, 0, "PROCESSED")
		err := insertWagerTransaction(t, ctx, conn, row)
		requireErrorCode(t, err, sqlstateCheckViolation, "OPENING with zero amount")
	})

	t.Run("OPENING with positive amount succeeds", func(t *testing.T) {
		conn := connectApp(t, ctx)
		w := newWallet(t, ctx, conn, 0)
		row := newOpeningTx(t, w, 1, "PROCESSED")
		requireNoError(t, insertWagerTransaction(t, ctx, conn, row), "OPENING with positive amount")
	})
}

// --- wallet_ledger_entries ---

func TestLedger_DuplicateWalletTransactionFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)
	tx := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, tx), "fixture: insert bet transaction")

	requireNoError(t, insertLedgerEntry(t, ctx, conn, newUUID(t), w.id, tx.id, "DEBIT", 5000, 10000, 5000, testCurrency), "first ledger entry")

	err := insertLedgerEntry(t, ctx, conn, newUUID(t), w.id, tx.id, "DEBIT", 5000, 5000, 0, testCurrency)
	requireErrorCode(t, err, sqlstateUniqueViolation, "duplicate (walletId, transactionId) in the ledger")
}

func TestLedger_IncoherentBalanceFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)
	tx := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, tx), "fixture: insert bet transaction")

	// A DEBIT of 5000 from 10000 must land on 5000, not 4000.
	err := insertLedgerEntry(t, ctx, conn, newUUID(t), w.id, tx.id, "DEBIT", 5000, 10000, 4000, testCurrency)
	requireErrorCode(t, err, sqlstateCheckViolation, "ledger entry whose balanceAfter disagrees with its direction")
}

// TestLedger_InsertByAppRoleSucceeds proves wallet_app can actually create
// a ledger entry: sequence_number is a GENERATED ALWAYS AS IDENTITY
// column, and inserting into it advances the identity's backing sequence,
// which wallet_app is granted USAGE on. Without that grant, this exact
// INSERT - the valid path every other ledger test in this file also
// depends on - is what fails.
func TestLedger_InsertByAppRoleSucceeds(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)
	tx := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, tx), "fixture: insert bet transaction")

	err := insertLedgerEntry(t, ctx, conn, newUUID(t), w.id, tx.id, "DEBIT", 5000, 10000, 5000, testCurrency)
	requireNoError(t, err, "INSERT into wallet_ledger_entries by wallet_app")
}

func TestLedger_UpdateFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)
	tx := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, tx), "fixture: insert bet transaction")
	entryID := newUUID(t)
	requireNoError(t, insertLedgerEntry(t, ctx, conn, entryID, w.id, tx.id, "DEBIT", 5000, 10000, 5000, testCurrency), "fixture: insert ledger entry")

	// wallet_app has no UPDATE grant on the ledger at all, so the write is
	// refused before the BEFORE UPDATE trigger even has a chance to run.
	_, err := conn.Exec(ctx, `UPDATE wallet_ledger_entries SET amount = 1 WHERE id = $1`, entryID)
	requireErrorCode(t, err, sqlstateInsufficientPrivilege, "UPDATE on the ledger by the application role")
}

func TestLedger_DeleteFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	w := newWallet(t, ctx, conn, 10000)
	tx := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, conn, tx), "fixture: insert bet transaction")
	entryID := newUUID(t)
	requireNoError(t, insertLedgerEntry(t, ctx, conn, entryID, w.id, tx.id, "DEBIT", 5000, 10000, 5000, testCurrency), "fixture: insert ledger entry")

	_, err := conn.Exec(ctx, `DELETE FROM wallet_ledger_entries WHERE id = $1`, entryID)
	requireErrorCode(t, err, sqlstateInsufficientPrivilege, "DELETE on the ledger by the application role")
}

func TestLedger_TruncateFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)

	_, err := conn.Exec(ctx, `TRUNCATE wallet_ledger_entries`)
	requireErrorCode(t, err, sqlstateInsufficientPrivilege, "TRUNCATE on the ledger by the application role")
}

// ledgerOwnerFixture inserts a bet transaction and one ledger entry via the
// app role, then hands back an owner connection and the entry id, for the
// three TestLedger_TriggerBlocks...EvenForOwner tests below - each proves
// the ledger's immutability does not rest on wallet_app's grants alone:
// the owner role, which genuinely does hold UPDATE, DELETE and TRUNCATE on
// every table it created, still cannot touch a ledger row, because the
// three BEFORE triggers refuse unconditionally, regardless of who is
// connected.
func ledgerOwnerFixture(t *testing.T, ctx context.Context) (ownerConn *pgx.Conn, entryID string) {
	t.Helper()
	appConn := connectApp(t, ctx)
	w := newWallet(t, ctx, appConn, 10000)
	tx := newExternalTx(t, w, "BET", 5000, "PROCESSED")
	requireNoError(t, insertWagerTransaction(t, ctx, appConn, tx), "fixture: insert bet transaction")
	entryID = newUUID(t)
	requireNoError(t, insertLedgerEntry(t, ctx, appConn, entryID, w.id, tx.id, "DEBIT", 5000, 10000, 5000, testCurrency), "fixture: insert ledger entry")
	return connectOwner(t, ctx), entryID
}

func TestLedger_TriggerBlocksUpdateEvenForOwner(t *testing.T) {
	ctx := context.Background()
	ownerConn, entryID := ledgerOwnerFixture(t, ctx)

	_, err := ownerConn.Exec(ctx, `UPDATE wallet_ledger_entries SET amount = 1 WHERE id = $1`, entryID)
	requireErrorCode(t, err, sqlstateRaiseException, "UPDATE on the ledger by the owner role")
}

func TestLedger_TriggerBlocksDeleteEvenForOwner(t *testing.T) {
	ctx := context.Background()
	ownerConn, entryID := ledgerOwnerFixture(t, ctx)

	_, err := ownerConn.Exec(ctx, `DELETE FROM wallet_ledger_entries WHERE id = $1`, entryID)
	requireErrorCode(t, err, sqlstateRaiseException, "DELETE on the ledger by the owner role")
}

func TestLedger_TriggerBlocksTruncateEvenForOwner(t *testing.T) {
	ctx := context.Background()
	ownerConn, _ := ledgerOwnerFixture(t, ctx)

	_, err := ownerConn.Exec(ctx, `TRUNCATE wallet_ledger_entries`)
	requireErrorCode(t, err, sqlstateRaiseException, "TRUNCATE on the ledger by the owner role")
}

// --- inbox_messages ---

func TestInboxMessages_DuplicateConsumerMessageFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	consumerName := "wager-consumer"
	messageID := newID(t, "message")

	requireNoError(t, insertInboxMessage(t, ctx, conn, newUUID(t), consumerName, messageID, "hash-1"), "first inbox message")

	err := insertInboxMessage(t, ctx, conn, newUUID(t), consumerName, messageID, "hash-2")
	requireErrorCode(t, err, sqlstateUniqueViolation, "duplicate (consumerName, messageId) in the inbox")
}

// --- outbox_events ---

func TestOutboxEvents_PayloadImmutableFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	eventID := newUUID(t)
	requireNoError(t, insertOutboxEvent(t, ctx, conn, eventID, newUUID(t), "WagerTransactionProcessed", `{"status":"PROCESSED"}`), "fixture: insert outbox event")
	markOutboxEventPublishedOnCleanup(t, eventID)

	_, err := conn.Exec(ctx, `UPDATE outbox_events SET payload = '{"status":"TAMPERED"}'::jsonb WHERE event_id = $1`, eventID)
	requireErrorCode(t, err, sqlstateRaiseException, "UPDATE of the outbox payload")
}

func TestOutboxEvents_EventTypeImmutableFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	eventID := newUUID(t)
	requireNoError(t, insertOutboxEvent(t, ctx, conn, eventID, newUUID(t), "WagerTransactionProcessed", `{"status":"PROCESSED"}`), "fixture: insert outbox event")
	markOutboxEventPublishedOnCleanup(t, eventID)

	_, err := conn.Exec(ctx, `UPDATE outbox_events SET event_type = 'WagerTransactionRejected' WHERE event_id = $1`, eventID)
	requireErrorCode(t, err, sqlstateRaiseException, "UPDATE of the outbox event_type")
}

func TestOutboxEvents_EventIDImmutableFails(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	eventID := newUUID(t)
	requireNoError(t, insertOutboxEvent(t, ctx, conn, eventID, newUUID(t), "WagerTransactionProcessed", `{"status":"PROCESSED"}`), "fixture: insert outbox event")
	markOutboxEventPublishedOnCleanup(t, eventID)

	_, err := conn.Exec(ctx, `UPDATE outbox_events SET event_id = $1 WHERE event_id = $2`, newUUID(t), eventID)
	requireErrorCode(t, err, sqlstateRaiseException, "UPDATE of the outbox event_id")
}

func TestOutboxEvents_PublishingLifecycleColumnsUpdateSucceeds(t *testing.T) {
	ctx := context.Background()
	conn := connectApp(t, ctx)
	eventID := newUUID(t)
	requireNoError(t, insertOutboxEvent(t, ctx, conn, eventID, newUUID(t), "WagerTransactionProcessed", `{"status":"PROCESSED"}`), "fixture: insert outbox event")
	markOutboxEventPublishedOnCleanup(t, eventID)

	_, err := conn.Exec(ctx, `
		UPDATE outbox_events
		SET attempts = attempts + 1, next_attempt_at = now(), locked_until = now(), published_at = now(), last_error = 'timeout'
		WHERE event_id = $1`, eventID)
	requireNoError(t, err, "updating the publishing-lifecycle columns of an outbox event")
}

func markOutboxEventPublishedOnCleanup(t *testing.T, eventID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		owner, err := pgx.Connect(ctx, ownerDSN(t))
		if err != nil {
			t.Errorf("connect migration owner to publish outbox fixture: %v", err)
			return
		}
		defer owner.Close(ctx)
		if _, err := owner.Exec(ctx, `UPDATE outbox_events SET published_at = now() WHERE event_id = $1`, eventID); err != nil {
			t.Errorf("mark outbox fixture published: %v", err)
		}
	})
}
