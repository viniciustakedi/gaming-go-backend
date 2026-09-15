-- Intentionally minimal: this ticket only has to prove the migrate
-- subcommand works end to end (up, down, up again) against a real
-- Postgres. The full domain schema - wallets, wager_transactions, the
-- ledger, inbox, outbox, roles and grants - is ticket 05's job, and lands
-- as migration 0002 onward.
CREATE TABLE schema_bootstrap (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    migrated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
