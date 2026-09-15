//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

const testCurrency = "BRL"

// sharedTestProviderID is used by every external transaction fixture in
// this package. It is safe to share across unrelated tests: the columns
// that actually distinguish rows - idempotency_key and
// external_transaction_id - are generated fresh per call by newID, so only
// tests that deliberately reuse one of those two values on purpose (to
// exercise the (providerId, ...) unique constraints) ever collide.
const sharedTestProviderID = "provider-schema-test"

type walletRow struct {
	id       string
	playerID string
	currency string
	balance  int64
	version  int64
}

// defaultResultingBalance mirrors wager_transactions_resulting_balance_terminal_check:
// PROCESSED and REJECTED rows require a non-negative resulting balance,
// PENDING_REFERENCE and FAILED (and anything else, such as the in-memory-only
// PENDING) forbid one. amount is reused as a convenient non-negative
// placeholder value - the constraint does not tie resulting_balance to any
// particular relationship with amount, only to status.
func defaultResultingBalance(status string, amount int64) *int64 {
	switch status {
	case "PROCESSED", "REJECTED":
		return int64Ptr(amount)
	default:
		return nil
	}
}

func insertWallet(t *testing.T, ctx context.Context, conn *pgx.Conn, w walletRow) error {
	t.Helper()
	_, err := conn.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance, version)
		VALUES ($1, $2, $3, $4, $5)`,
		w.id, w.playerID, w.currency, w.balance, w.version)
	return err
}

// newWallet inserts a fresh wallet with a unique id and player and fails
// the test immediately if the insert itself does not succeed: every test
// that calls it is proving something about a *different* table, so a
// failure here would be a fixture bug, not the behaviour under test.
func newWallet(t *testing.T, ctx context.Context, conn *pgx.Conn, balance int64) walletRow {
	t.Helper()
	w := walletRow{
		id:       newUUID(t),
		playerID: newUUID(t),
		currency: testCurrency,
		balance:  balance,
		version:  1,
	}
	requireNoError(t, insertWallet(t, ctx, conn, w), "fixture: insert wallet")
	return w
}

type wagerTxRow struct {
	id                     string
	externalTransactionID  *string
	providerID             *string
	idempotencyKey         *string
	payloadHash            *string
	walletID               string
	playerID               string
	roundID                *string
	gameID                 *string
	kind                   string
	origin                 string
	amount                 int64
	currency               string
	referenceExternalID    *string
	referenceTransactionID *string
	status                 string
	failureCode            *string
	resultingBalance       *int64
}

func insertWagerTransaction(t *testing.T, ctx context.Context, conn *pgx.Conn, r wagerTxRow) error {
	t.Helper()
	_, err := conn.Exec(ctx, `
		INSERT INTO wager_transactions (
			id, external_transaction_id, provider_id, idempotency_key, payload_hash,
			wallet_id, player_id, round_id, game_id, kind, origin, amount, currency,
			reference_external_transaction_id, reference_transaction_id, status, failure_code,
			resulting_balance
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		r.id, r.externalTransactionID, r.providerID, r.idempotencyKey, r.payloadHash,
		r.walletID, r.playerID, r.roundID, r.gameID, r.kind, r.origin, r.amount, r.currency,
		r.referenceExternalID, r.referenceTransactionID, r.status, r.failureCode,
		r.resultingBalance)
	return err
}

func strPtr(s string) *string { return &s }

// newOpeningTx returns a valid INTERNAL OPENING row for wallet w - the
// only origin/kind pair the origin_external_columns_check allows to carry
// no provider metadata at all.
func newOpeningTx(t *testing.T, w walletRow, amount int64, status string) wagerTxRow {
	t.Helper()
	return wagerTxRow{
		id:               newUUID(t),
		walletID:         w.id,
		playerID:         w.playerID,
		kind:             "OPENING",
		origin:           "INTERNAL",
		amount:           amount,
		currency:         w.currency,
		status:           status,
		resultingBalance: defaultResultingBalance(status, amount),
	}
}

// newExternalTx returns a valid EXTERNAL row for wallet w with every
// required provider-metadata column filled and a unique idempotency key
// and external transaction id, so unrelated tests never collide on the
// (providerId, idempotencyKey) or (providerId, externalTransactionId)
// unique constraints unless a test deliberately overrides one to do so.
func newExternalTx(t *testing.T, w walletRow, kind string, amount int64, status string) wagerTxRow {
	t.Helper()
	return wagerTxRow{
		id:                    newUUID(t),
		externalTransactionID: strPtr(newID(t, "ext")),
		providerID:            strPtr(sharedTestProviderID),
		idempotencyKey:        strPtr(newID(t, "idem")),
		payloadHash:           strPtr(newID(t, "hash")),
		walletID:              w.id,
		playerID:              w.playerID,
		roundID:               strPtr(newID(t, "round")),
		gameID:                strPtr(newID(t, "game")),
		kind:                  kind,
		origin:                "EXTERNAL",
		amount:                amount,
		currency:              w.currency,
		status:                status,
		resultingBalance:      defaultResultingBalance(status, amount),
	}
}

func insertLedgerEntry(t *testing.T, ctx context.Context, conn *pgx.Conn, id, walletID, transactionID, direction string, amount, balanceBefore, balanceAfter int64, currency string) error {
	t.Helper()
	_, err := conn.Exec(ctx, `
		INSERT INTO wallet_ledger_entries (
			id, wallet_id, transaction_id, direction, amount, currency, balance_before, balance_after, occurred_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
		id, walletID, transactionID, direction, amount, currency, balanceBefore, balanceAfter)
	return err
}

func insertInboxMessage(t *testing.T, ctx context.Context, conn *pgx.Conn, id, consumerName, messageID, payloadHash string) error {
	t.Helper()
	_, err := conn.Exec(ctx, `
		INSERT INTO inbox_messages (id, consumer_name, message_id, payload_hash, received_at)
		VALUES ($1, $2, $3, $4, now())`,
		id, consumerName, messageID, payloadHash)
	return err
}

func insertOutboxEvent(t *testing.T, ctx context.Context, conn *pgx.Conn, eventID, aggregateID, eventType, payload string) error {
	t.Helper()
	_, err := conn.Exec(ctx, `
		INSERT INTO outbox_events (
			event_id, aggregate_type, aggregate_id, event_type, event_version, payload, occurred_at
		) VALUES ($1, 'WagerTransaction', $2, $3, 1, $4::jsonb, now())`,
		eventID, aggregateID, eventType, payload)
	return err
}
