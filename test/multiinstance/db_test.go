//go:build multiinstance

package multiinstance

import (
	"context"
	"testing"

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
