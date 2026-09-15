CREATE TABLE wallet_ledger_entries (
    id              UUID NOT NULL CONSTRAINT wallet_ledger_entries_id_pk PRIMARY KEY,
    wallet_id       UUID NOT NULL CONSTRAINT wallet_ledger_entries_wallet_id_fk REFERENCES wallets (id),
    transaction_id  UUID NOT NULL CONSTRAINT wallet_ledger_entries_transaction_id_fk REFERENCES wager_transactions (id),
    direction       TEXT NOT NULL,
    amount          BIGINT NOT NULL,
    currency        CHAR(3) NOT NULL,
    balance_before  BIGINT NOT NULL,
    balance_after   BIGINT NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    sequence_number BIGINT GENERATED ALWAYS AS IDENTITY,

    CONSTRAINT wallet_ledger_entries_direction_check CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT wallet_ledger_entries_amount_check CHECK (amount > 0),
    CONSTRAINT wallet_ledger_entries_balance_before_check CHECK (balance_before >= 0),
    CONSTRAINT wallet_ledger_entries_balance_after_check CHECK (balance_after >= 0),

    CONSTRAINT wallet_ledger_entries_wallet_id_transaction_id_key UNIQUE (wallet_id, transaction_id),
    CONSTRAINT wallet_ledger_entries_sequence_number_key UNIQUE (sequence_number),

    -- This is NewWalletLedgerEntry's own balance equation, enforced a
    -- second time by the table that is supposed to be the append-only
    -- proof of every balance movement: a CREDIT must land on
    -- balance_before + amount, a DEBIT on balance_before - amount. Nothing
    -- else is a valid ledger row, no matter what inserted it.
    CONSTRAINT wallet_ledger_entries_balance_direction_check CHECK (
        (direction = 'CREDIT' AND balance_after = balance_before + amount)
        OR
        (direction = 'DEBIT' AND balance_after = balance_before - amount)
    )
);

-- sequence_number is what "coluna de sequência para ordenação estável"
-- asks for: an IDENTITY column that only ever moves forward, so a ledger
-- page can resume exactly where the last one stopped without skipping or
-- repeating a row even if timestamps collide. This index is the one a
-- paginated "list this wallet's ledger in sequence order" query needs.
CREATE INDEX wallet_ledger_entries_wallet_sequence_idx
    ON wallet_ledger_entries (wallet_id, sequence_number);

-- Grants stop at SELECT and INSERT on purpose - the spec is explicit that
-- the ledger is the one table where the application role gets nothing else
-- ("No ledger, o papel da aplicação tem só SELECT e INSERT"). The triggers
-- below are the second, independent layer: even if some future migration
-- accidentally granted UPDATE or DELETE here, the row would still refuse
-- to change.
GRANT SELECT, INSERT ON wallet_ledger_entries TO wallet_app;

-- GENERATED ALWAYS AS IDENTITY names its backing sequence
-- "<table>_<column>_seq" (here wallet_ledger_entries_sequence_number_seq).
-- INSERT privilege on the table is enough for Postgres to advance an
-- identity column's own sequence implicitly, but wallet_app is granted
-- USAGE on it explicitly and exactly - never SELECT or UPDATE - so the
-- role's privileges on this table read the same way in \dp as everywhere
-- else: exactly what the spec lists, no more.
GRANT USAGE ON SEQUENCE wallet_ledger_entries_sequence_number_seq TO wallet_app;

-- One function serves UPDATE, DELETE and TRUNCATE: TG_OP tells them apart
-- for the error message, but all three are refused unconditionally,
-- because the ledger has no legitimate use for any of them - it is
-- append-only for its entire life, not just while a row is "recent".
CREATE FUNCTION wallet_ledger_entries_block_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries: append-only, % is not allowed', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallet_ledger_entries_block_update
    BEFORE UPDATE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_block_mutation();

CREATE TRIGGER wallet_ledger_entries_block_delete
    BEFORE DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_block_mutation();

-- Statement-level, not row-level: TRUNCATE never fires row triggers. It
-- also requires its own separate TRUNCATE privilege, which wallet_app is
-- never granted, so this trigger is the belt matching that suspenders -
-- both layers have to be bypassed, not just one, before the ledger could
-- ever be truncated.
CREATE TRIGGER wallet_ledger_entries_block_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION wallet_ledger_entries_block_mutation();
