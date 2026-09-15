//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/migrate"
	"github.com/viniciustakedi/jungle-gaming-wallet/migrations"
)

// TestMigrations_UpDownUp proves the full migration chain - roles, wallets,
// wager_transactions, the ledger, inbox and outbox - reverses cleanly and
// reapplies cleanly, per the spec ("Todas as migrations sobem, descem e
// sobem de novo").
//
// It runs against the same database every other test in this package
// uses, tearing every wallet-related table down and rebuilding it midway
// through. That is safe only because nothing in this package calls
// t.Parallel(): Go runs every test in a package to completion, one at a
// time, before starting the next, so no other test's fixtures are ever
// live while this one drops and recreates the schema.
func TestMigrations_UpDownUp(t *testing.T) {
	dsn := ownerDSN(t)
	ctx := context.Background()

	// The provisioning flow this suite depends on (README.md: `docker
	// compose up -d postgres ministack provisioning postgres-provisioning
	// migrate`) has already applied every migration once, so the starting
	// point is "up".
	assertWalletsTableExists(t, ctx, dsn, true)

	if err := migrate.Down(dsn); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	assertWalletsTableExists(t, ctx, dsn, false)

	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	assertWalletsTableExists(t, ctx, dsn, true)

	// wallet_app is a cluster object provisioning owns, never a migration
	// (see migrations/0002_app_role.up.sql/.down.sql) - the down/up cycle
	// above must never touch its password. Proved here by connecting as
	// wallet_app with exactly the password deploy/postgres/provision.sh
	// wrote before this test ran, and exercising a real write, not just a
	// successful connection.
	assertWalletAppCanInsertWallet(t, ctx)
}

// assertWalletAppCanInsertWallet proves wallet_app authenticates with its
// provisioned password and can write through the grants the down/up cycle
// just reapplied - the same INSERT any other seam-2 schema test in this
// package relies on wallet_app being able to perform.
func assertWalletAppCanInsertWallet(t *testing.T, ctx context.Context) {
	t.Helper()
	conn := connectApp(t, ctx)
	_, err := conn.Exec(ctx,
		`INSERT INTO wallets (id, player_id, currency, balance, version) VALUES ($1, $2, $3, $4, $5)`,
		newUUID(t), newUUID(t), "USD", int64(0), int64(1),
	)
	requireNoError(t, err, "INSERT into wallets by wallet_app after migrate down/up")
}

// TestMigrations_UpFailsWhenRoleMissing proves migrations/0002_app_role.up.sql
// refuses to run, with a clear message, against a Postgres where the
// wallet_app role does not exist yet - the failure mode this ticket
// requires instead of the migration silently creating the role itself.
//
// wallet_app is a cluster-wide role (see migrations/0002_app_role.up.sql),
// so a disposable database on its own cannot make it "not exist" - every
// database in this cluster already has it, courtesy of
// deploy/postgres/provision.sh. Isolation instead comes from a
// configurable role name: this test executes 0002's own SQL, verbatim
// except for the role name, against a disposable database, substituting a
// randomly generated role name that is guaranteed to exist nowhere in the
// cluster. That proves the same DO block real migrations run, with no
// changes to the production migration or to the shared wallet_app role.
func TestMigrations_UpFailsWhenRoleMissing(t *testing.T) {
	ctx := context.Background()
	owner := connectOwner(t, ctx)

	suffix := randomHex(t)
	dbName := "wallet_missing_role_test_" + suffix
	if _, err := owner.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{dbName}.Sanitize()); err != nil {
		t.Fatalf("create disposable database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := owner.Exec(ctx, `DROP DATABASE `+pgx.Identifier{dbName}.Sanitize()); err != nil {
			t.Errorf("drop disposable database: %v", err)
		}
	})

	dsn := dsnWithDatabase(t, ownerDSN(t), dbName)
	scratch := connect(t, ctx, dsn)

	missingRole := "wallet_app_missing_" + suffix
	sql, err := migrations.FS.ReadFile("0002_app_role.up.sql")
	if err != nil {
		t.Fatalf("read 0002_app_role.up.sql: %v", err)
	}
	templated := strings.ReplaceAll(string(sql), "wallet_app", missingRole)

	_, err = scratch.Exec(ctx, templated)
	requireErrorCode(t, err, sqlstateRaiseException, "0002_app_role.up.sql against a database with no "+missingRole+" role")
	if !strings.Contains(err.Error(), "ausente") || !strings.Contains(err.Error(), "provisionamento") {
		t.Fatalf("migration error message = %q, want it to mention the missing role and provisioning", err.Error())
	}
}

func assertWalletsTableExists(t *testing.T, ctx context.Context, dsn string, want bool) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()

	var exists bool
	err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'wallets')`).Scan(&exists)
	if err != nil {
		t.Fatalf("check wallets table: %v", err)
	}
	if exists != want {
		t.Fatalf("wallets table exists = %v, want %v", exists, want)
	}
}
