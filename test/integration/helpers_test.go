//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const defaultOwnerDSN = "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable"

// Postgres SQLSTATE codes this package asserts on directly - decision 8 of
// the alignment restricts this repository's dependencies, and pgerrcode is
// not among them, so these are spelled out by hand instead of imported.
// See https://www.postgresql.org/docs/current/errcodes-appendix.html.
const (
	sqlstateCheckViolation        = "23514"
	sqlstateUniqueViolation       = "23505"
	sqlstateForeignKeyViolation   = "23503"
	sqlstateInsufficientPrivilege = "42501"
	sqlstateRaiseException        = "P0001"
)

// ownerDSN is the migration owner's connection string - DATABASE_URL, the
// same variable `migrate up`/`down` and every other integration test in
// this repo already read (see README.md). It is used in this package only
// for setup that genuinely needs elevated privileges: running the
// migration cycle and proving the ledger triggers hold even for a role
// that does have UPDATE/DELETE/TRUNCATE granted.
func ownerDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn
	}
	return defaultOwnerDSN
}

// appDSN reuses ownerDSN's host, port, database and sslmode, and swaps in
// wallet_app's own credentials - the role every seam 2 scenario in this
// package connects as, per the ticket ("SQL cru executado pelo papel da
// aplicação"). Deriving it from DATABASE_URL instead of a second env var
// means these tests need no configuration beyond what the rest of the
// suite already requires. The password itself is never versioned or
// hardcoded here: it is generated and reused by
// deploy/postgres/provision.sh (see README.md) and read from the same
// runtime credentials file walletAppPassword loads below.
func appDSN(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(ownerDSN(t))
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	u.User = url.UserPassword("wallet_app", walletAppPassword(t))
	return u.String()
}

// walletAppPassword reads WALLET_APP_PASSWORD from
// deploy/postgres/.runtime/credentials.env - the file
// deploy/postgres/provision.sh writes on every `docker compose up`, the
// same one-file-per-role, 0600, git-ignored pattern
// deploy/ministack/provision.sh already uses for the SQS role keys (see
// iam_test.go's loadTestCreds and README.md). POSTGRES_CREDENTIALS_FILE
// overrides the default path, matching APP_CREDENTIALS_FILE/
// TEST_CREDENTIALS_FILE's own override convention.
func walletAppPassword(t *testing.T) string {
	t.Helper()
	path := os.Getenv("POSTGRES_CREDENTIALS_FILE")
	if path == "" {
		path = filepath.Join("..", "..", "deploy", "postgres", ".runtime", "credentials.env")
	}
	values := map[string]string{}
	readEnvFile(t, path, values)
	password, ok := values["WALLET_APP_PASSWORD"]
	if !ok || password == "" {
		t.Fatalf("missing WALLET_APP_PASSWORD in %s - run `docker compose up postgres-provisioning` first, see README.md", path)
	}
	return password
}

// dsnWithDatabase reuses dsn's host, port, credentials and query string,
// swapping in a different database name - used to connect to a disposable
// database created inside the same Postgres instance.
func dsnWithDatabase(t *testing.T, dsn, database string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + database
	return u.String()
}

func connect(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close connection: %v", err)
		}
	})
	return conn
}

func connectApp(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	return connect(t, ctx, appDSN(t))
}

func connectOwner(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	return connect(t, ctx, ownerDSN(t))
}

// newID returns a short, random, collision-free identifier for the opaque
// external TEXT columns of this schema (provider ids, external transaction
// ids, idempotency keys, round/game ids, message ids and the like). These
// only need uniqueness across runs, not any particular shape.
func newID(t *testing.T, prefix string) string {
	t.Helper()
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("generate id: %v", err)
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(buf[:]))
}

// newUUID returns a random version-4 UUID string for the internal
// identifier columns this schema types as UUID (wallet, player, wager
// transaction and ledger entry ids, and the outbox's event/aggregate ids).
// The domain generates real ids as UUID v7 (see alignment.md); a hand
// -rolled v4 generator needs only crypto/rand, already imported here, and
// avoids pulling in a UUID dependency the alignment decisions never called
// for - these tests only need a syntactically valid, unique UUID, not any
// particular version's bit layout.
func newUUID(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// randomHex returns a short, random, lowercase hex string safe to splice
// into an unquoted SQL identifier (a database or role name) - unlike
// newID, it carries no separator character that would make the result an
// invalid unquoted identifier.
func randomHex(t *testing.T) string {
	t.Helper()
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("generate random hex: %v", err)
	}
	return hex.EncodeToString(buf[:])
}

func int64Ptr(v int64) *int64 { return &v }

func pgErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// requireErrorCode fails the test unless err is a Postgres error carrying
// exactly the given SQLSTATE - the independent source of truth here is the
// error code itself, not a substring match on a message that could change.
func requireErrorCode(t *testing.T, err error, want, scenario string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want error code %s, got nil", scenario, want)
	}
	if got := pgErrorCode(err); got != want {
		t.Fatalf("%s: want error code %s, got %q (err: %v)", scenario, want, got, err)
	}
}

func requireNoError(t *testing.T, err error, scenario string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: want no error, got %v", scenario, err)
	}
}
