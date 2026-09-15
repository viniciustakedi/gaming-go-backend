//go:build multiinstance

package multiinstance

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// connectApp opens a direct connection to Postgres as wallet_app, used only
// to read back state for assertions - never to move money itself. The
// financial result of every scenario in this package comes from the
// instances under test over HTTP; this connection exists purely to check
// it against an independent source of truth (spec, Testing Decisions: "Todo
// cenário financeiro termina conferindo o saldo armazenado contra a soma de
// créditos menos débitos do ledger").
func connectApp(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, appDSN(t))
	if err != nil {
		t.Fatalf("connect to postgres as wallet_app: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close postgres connection: %v", err)
		}
	})
	return conn
}

func queryWalletBalance(t *testing.T, ctx context.Context, conn *pgx.Conn, walletID string) int64 {
	t.Helper()
	var balance int64
	if err := conn.QueryRow(ctx, `SELECT balance FROM wallets WHERE id = $1`, walletID).Scan(&balance); err != nil {
		t.Fatalf("query wallet balance: %v", err)
	}
	return balance
}

func countLedgerEntries(t *testing.T, ctx context.Context, conn *pgx.Conn, walletID string) int {
	t.Helper()
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&count); err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}
	return count
}

// netLedgerBalance sums credits minus debits for walletID straight from the
// ledger - the independent source of truth every scenario checks the
// stored wallet balance against, computed by hand-written SQL rather than
// by calling back into any code path the application itself uses.
func netLedgerBalance(t *testing.T, ctx context.Context, conn *pgx.Conn, walletID string) int64 {
	t.Helper()
	var net int64
	err := conn.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN direction = 'CREDIT' THEN amount ELSE -amount END), 0)
		FROM wallet_ledger_entries
		WHERE wallet_id = $1`, walletID).Scan(&net)
	if err != nil {
		t.Fatalf("sum ledger entries: %v", err)
	}
	return net
}

// findTransactionByExternalID reads back the durable status a wager
// transaction settled at, for assertions after a fault-injected crash - the
// independent way to prove a killed process left nothing behind (found =
// false) or committed exactly what the scenario expects.
func findTransactionByExternalID(t *testing.T, ctx context.Context, conn *pgx.Conn, externalTransactionID string) (id, status string, found bool) {
	t.Helper()
	err := conn.QueryRow(ctx, `SELECT id, status FROM wager_transactions WHERE external_transaction_id = $1`, externalTransactionID).Scan(&id, &status)
	if err == pgx.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("find wager transaction by external id: %v", err)
	}
	return id, status, true
}

// waitForTransactionStatus polls findTransactionByExternalID until it
// observes want or the deadline passes - used after a fault-injected crash,
// where reaching the expected terminal state depends on another instance's
// worker or redelivered message, not on this test's own call returning.
func waitForTransactionStatus(t *testing.T, ctx context.Context, conn *pgx.Conn, externalTransactionID, want string, timeout time.Duration) (id string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if gotID, status, found := findTransactionByExternalID(t, ctx, conn, externalTransactionID); found && status == want {
			return gotID
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, status, found := findTransactionByExternalID(t, ctx, conn, externalTransactionID)
	t.Fatalf("wager transaction %s status = %q (found %v) before %s deadline, want %q", externalTransactionID, status, found, timeout, want)
	return ""
}

// waitForWalletBalance polls queryWalletBalance until it observes want or
// timeout passes.
func waitForWalletBalance(t *testing.T, ctx context.Context, conn *pgx.Conn, walletID string, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := queryWalletBalance(t, ctx, conn, walletID); got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("wallet %s balance = %d before %s deadline, want %d", walletID, queryWalletBalance(t, ctx, conn, walletID), timeout, want)
}

// openingTransactionID reads the id of walletID's own OPENING transaction -
// the aggregate id its WagerTransactionProcessed outbox event was recorded
// under (spec: "WagerTransactionProcessed... cobre conclusões com sucesso,
// incluindo... OPENING").
func openingTransactionID(t *testing.T, ctx context.Context, conn *pgx.Conn, walletID string) string {
	t.Helper()
	var id string
	if err := conn.QueryRow(ctx, `SELECT id FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING'`, walletID).Scan(&id); err != nil {
		t.Fatalf("find opening transaction for wallet %s: %v", walletID, err)
	}
	return id
}

// outboxEventIDs reads the eventIds committed for aggregateID, in insertion
// order - the fixed set of eventIds an outbox fault-injection scenario
// tracks on the output queue, read independently of whichever publisher
// instance ends up sending them.
func outboxEventIDs(t *testing.T, ctx context.Context, conn *pgx.Conn, aggregateID string) []string {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT event_id FROM outbox_events WHERE aggregate_id = $1 ORDER BY occurred_at`, aggregateID)
	if err != nil {
		t.Fatalf("query outbox event ids: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan outbox event id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox event ids: %v", err)
	}
	return ids
}

// openingOutboxEventIDs returns the eventIds of both outbox records a
// positive-balance wallet opening commits (spec, "Outbox e eventos"):
// WagerTransactionProcessed, aggregated under the OPENING transaction, and
// WalletBalanceChanged, aggregated under the wallet itself.
func openingOutboxEventIDs(t *testing.T, ctx context.Context, conn *pgx.Conn, walletID string) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, id := range outboxEventIDs(t, ctx, conn, walletID) {
		ids[id] = true
	}
	for _, id := range outboxEventIDs(t, ctx, conn, openingTransactionID(t, ctx, conn, walletID)) {
		ids[id] = true
	}
	return ids
}

// outboxPublished reports whether eventID has been confirmed published.
func outboxPublished(t *testing.T, ctx context.Context, conn *pgx.Conn, eventID string) bool {
	t.Helper()
	var publishedAt *time.Time
	if err := conn.QueryRow(ctx, `SELECT published_at FROM outbox_events WHERE event_id = $1`, eventID).Scan(&publishedAt); err != nil {
		t.Fatalf("read outbox published_at: %v", err)
	}
	return publishedAt != nil
}

// waitForOutboxPublished polls outboxPublished until it observes true or
// timeout passes.
func waitForOutboxPublished(t *testing.T, ctx context.Context, conn *pgx.Conn, eventID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if outboxPublished(t, ctx, conn, eventID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("outbox event %s not confirmed published before %s deadline", eventID, timeout)
}

// pgNow reads Postgres's own wall clock - the "since" boundary
// outboxPublishedAfter compares against, so that check never depends on the
// test host's clock being in sync with the database server's.
func pgNow(t *testing.T, ctx context.Context, conn *pgx.Conn) time.Time {
	t.Helper()
	var now time.Time
	if err := conn.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("read postgres now(): %v", err)
	}
	return now
}

// outboxPublishedAfter reports whether eventID's published_at is strictly
// after since - the independent, DB-side proof that whichever instance
// confirmed it did so only after since (a scenario passes the moment the
// victim is known dead, per waitExit, as its own since), so no other
// process could have been the one to complete it (review, correctness:
// "as tentativas e a confirmação no registro da outbox depois da morte").
func outboxPublishedAfter(t *testing.T, ctx context.Context, conn *pgx.Conn, eventID string, since time.Time) bool {
	t.Helper()
	var publishedAt *time.Time
	if err := conn.QueryRow(ctx, `SELECT published_at FROM outbox_events WHERE event_id = $1`, eventID).Scan(&publishedAt); err != nil {
		t.Fatalf("read outbox published_at: %v", err)
	}
	return publishedAt != nil && publishedAt.After(since)
}

// outboxPublishedAt reads eventID's own published_at - set by MarkPublished's
// `now()`, Postgres's own clock, never a test-host or instance wall clock -
// failing the test if it has not been confirmed yet. The dispute scenario's
// overlap proof (review, correctness: "com o relógio do Postgres") is built
// entirely on timestamps read through this function, so two instances'
// claimed spans are always comparable on the same clock even though they run
// as separate processes.
func outboxPublishedAt(t *testing.T, ctx context.Context, conn *pgx.Conn, eventID string) time.Time {
	t.Helper()
	var publishedAt *time.Time
	if err := conn.QueryRow(ctx, `SELECT published_at FROM outbox_events WHERE event_id = $1`, eventID).Scan(&publishedAt); err != nil {
		t.Fatalf("read outbox published_at: %v", err)
	}
	if publishedAt == nil {
		t.Fatalf("outbox event %s has no published_at yet", eventID)
	}
	return *publishedAt
}

// publishedSpan is the interval between the first and the last confirmed
// publish, on Postgres's own clock, among a set of eventIds - the
// per-instance evidence the dispute scenario's overlap assertion compares.
type publishedSpan struct {
	start, end time.Time
}

// overlaps reports whether s and other share any instant - true genuine
// concurrency between the two instances they each summarize, false for two
// spans that only ever ran one after the other.
func (s publishedSpan) overlaps(other publishedSpan) bool {
	return !s.start.After(other.end) && !other.start.After(s.end)
}

// instancePublishedSpan restricts eventIDs to the ones inst actually
// confirmed (instancePublishedEventIDs) and returns their published_at span.
// It fails the test if inst confirmed none of them - the overlap assertion
// this feeds needs both instances to have actually published something
// first.
func instancePublishedSpan(t *testing.T, ctx context.Context, conn *pgx.Conn, inst *instance, eventIDs map[string]bool) publishedSpan {
	t.Helper()
	mine := instancePublishedEventIDs(t, inst)
	var span publishedSpan
	found := false
	for eventID := range eventIDs {
		if !mine[eventID] {
			continue
		}
		at := outboxPublishedAt(t, ctx, conn, eventID)
		if !found || at.Before(span.start) {
			span.start = at
		}
		if !found || at.After(span.end) {
			span.end = at
		}
		found = true
	}
	if !found {
		t.Fatalf("instance %s confirmed none of the batch's events", inst.name)
	}
	return span
}

// outboxEventTypesFor reads the event_type of every outbox record committed
// for aggregateID, in insertion order - used to prove a specific event type
// (such as WagerTransactionRejected) was actually recorded, not just that
// some event was.
func outboxEventTypesFor(t *testing.T, ctx context.Context, conn *pgx.Conn, aggregateID string) []string {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY occurred_at`, aggregateID)
	if err != nil {
		t.Fatalf("query outbox event types: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var eventType string
		if err := rows.Scan(&eventType); err != nil {
			t.Fatalf("scan outbox event type: %v", err)
		}
		types = append(types, eventType)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox event types: %v", err)
	}
	return types
}

// transactionFailureCode reads the durable failure_code column for
// transactionID - nil when the transaction never failed.
func transactionFailureCode(t *testing.T, ctx context.Context, conn *pgx.Conn, transactionID string) *string {
	t.Helper()
	var code *string
	if err := conn.QueryRow(ctx, `SELECT failure_code FROM wager_transactions WHERE id = $1`, transactionID).Scan(&code); err != nil {
		t.Fatalf("read transaction failure code: %v", err)
	}
	return code
}
