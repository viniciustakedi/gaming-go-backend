// Package migrate wraps golang-migrate so the `migrate` subcommand and the
// one-shot Compose migrations service share the exact same up/down logic.
package migrate

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	// Registered for its side effect: it makes database/sql aware of the
	// "pgx" driver name, so sql.Open below uses pgx's own stdlib adapter
	// instead of pulling in lib/pq.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/viniciustakedi/jungle-gaming-wallet/migrations"
)

func newMigrator(dsn string) (*migrate.Migrate, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("migrate: open database/sql handle: %w", err)
	}

	dbDriver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: create pgx driver: %w", err)
	}

	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: load embedded migrations: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx5", dbDriver)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: connect: %w", err)
	}
	return m, nil
}

// Up applies every migration that has not run yet.
func Up(dsn string) (err error) {
	m, buildErr := newMigrator(dsn)
	if buildErr != nil {
		return buildErr
	}
	defer func() { err = errors.Join(err, closeMigrator(m)) }()

	if upErr := m.Up(); upErr != nil && !errors.Is(upErr, migrate.ErrNoChange) {
		return fmt.Errorf("migrate: up: %w", upErr)
	}
	return nil
}

// Down rolls back every applied migration.
func Down(dsn string) (err error) {
	m, buildErr := newMigrator(dsn)
	if buildErr != nil {
		return buildErr
	}
	defer func() { err = errors.Join(err, closeMigrator(m)) }()

	if downErr := m.Down(); downErr != nil && !errors.Is(downErr, migrate.ErrNoChange) {
		return fmt.Errorf("migrate: down: %w", downErr)
	}
	return nil
}

// closeMigrator joins both errors m.Close returns (source, database) instead
// of discarding them, so a deferred call can fold them into the caller's
// named result without masking whatever error already happened there.
func closeMigrator(m *migrate.Migrate) error {
	srcErr, dbErr := m.Close()
	return errors.Join(srcErr, dbErr)
}
