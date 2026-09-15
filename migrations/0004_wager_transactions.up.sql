CREATE TABLE wager_transactions (
    id                                 UUID NOT NULL CONSTRAINT wager_transactions_id_pk PRIMARY KEY,
    external_transaction_id            TEXT,
    provider_id                        TEXT,
    idempotency_key                    TEXT,
    payload_hash                       TEXT,
    wallet_id                          UUID NOT NULL CONSTRAINT wager_transactions_wallet_id_fk REFERENCES wallets (id),
    player_id                          UUID NOT NULL,
    round_id                           TEXT,
    game_id                            TEXT,
    kind                               TEXT NOT NULL,
    origin                             TEXT NOT NULL,
    amount                             BIGINT NOT NULL,
    currency                           CHAR(3) NOT NULL,
    reference_external_transaction_id  TEXT,
    reference_transaction_id           UUID CONSTRAINT wager_transactions_reference_transaction_id_fk REFERENCES wager_transactions (id),
    status                             TEXT NOT NULL,
    failure_code                       TEXT,
    resulting_balance                  BIGINT,
    attempts                           INT NOT NULL DEFAULT 0,
    next_attempt_at                    TIMESTAMPTZ,
    pending_expires_at                 TIMESTAMPTZ,
    created_at                         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT wager_transactions_kind_check
        CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    -- PENDING only ever exists in memory, before the first INSERT (spec:
    -- "processamento síncrono"); the only non-terminal status a row is
    -- ever persisted with is PENDING_REFERENCE, so PENDING itself is
    -- rejected here - a raw INSERT cannot create a row no worker will ever
    -- pick up.
    CONSTRAINT wager_transactions_status_check
        CHECK (status IN ('PROCESSED', 'REJECTED', 'FAILED', 'PENDING_REFERENCE')),
    CONSTRAINT wager_transactions_origin_check
        CHECK (origin IN ('INTERNAL', 'EXTERNAL')),

    -- Mirrors WagerTransaction.validate() in the domain: an INTERNAL row is
    -- always the OPENING credit and carries none of the provider metadata,
    -- because there is no provider, idempotency key or payload to
    -- deduplicate against. An EXTERNAL row is always a provider operation
    -- and always carries all of it, because every one of those columns is
    -- how the operation gets deduplicated and audited back to its provider.
    CONSTRAINT wager_transactions_origin_external_columns_check CHECK (
        (origin = 'INTERNAL' AND kind = 'OPENING'
            AND external_transaction_id IS NULL AND provider_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL
            AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL
            AND reference_transaction_id IS NULL)
        OR
        (origin = 'EXTERNAL' AND kind <> 'OPENING'
            AND external_transaction_id IS NOT NULL AND provider_id IS NOT NULL
            AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL AND game_id IS NOT NULL)
    ),

    -- REFUND and ROLLBACK are meaningless without something to reverse;
    -- BET, WIN and LOSS never carry one (WIN's reference, when present, is
    -- optional, so it is untouched by this rule either way).
    CONSTRAINT wager_transactions_reversal_requires_reference_check
        CHECK (kind NOT IN ('REFUND', 'ROLLBACK') OR reference_external_transaction_id IS NOT NULL),

    -- failure_code is the durable audit code for why a REJECTED or FAILED
    -- row ended that way, and is meaningless - so forbidden - in any other
    -- status, including PENDING_REFERENCE, which is not itself a failure.
    CONSTRAINT wager_transactions_failure_code_terminal_check
        CHECK ((status IN ('REJECTED', 'FAILED')) = (failure_code IS NOT NULL)),

    -- The spec requires a stable financial result for replay: PROCESSED
    -- and REJECTED are the two statuses the provider receives a resulting
    -- balance for, so both require one, not negative. PENDING_REFERENCE
    -- and FAILED have no financial result yet (or ever), so both forbid
    -- one - and the terminal-row trigger below means a row can never drift
    -- from whichever side of this it landed on.
    CONSTRAINT wager_transactions_resulting_balance_terminal_check CHECK (
        (status IN ('PROCESSED', 'REJECTED') AND resulting_balance IS NOT NULL AND resulting_balance >= 0)
        OR
        (status IN ('PENDING_REFERENCE', 'FAILED') AND resulting_balance IS NULL)
    ),

    -- Defense in depth for the per-kind value policy in "Regras das
    -- operações e referências": BET, WIN, REFUND, ROLLBACK and OPENING all
    -- move money and must be strictly positive; LOSS moves nothing and
    -- must be exactly zero. This is the bank's own copy of a rule the
    -- domain already enforces, so raw SQL through wallet_app cannot record
    -- a zero-value BET or a non-zero LOSS.
    CONSTRAINT wager_transactions_amount_by_kind_check CHECK (
        (kind IN ('BET', 'WIN', 'REFUND', 'ROLLBACK', 'OPENING') AND amount > 0)
        OR
        (kind = 'LOSS' AND amount = 0)
    ),

    -- Idempotency is scoped per provider (spec: "o escopo de chaves é por
    -- provedor"). Both pairs are NULL for INTERNAL rows, and Postgres never
    -- treats a pair of NULLs as a duplicate, so OPENING rows never collide
    -- here - only EXTERNAL rows, which always populate both, actually
    -- exercise these constraints.
    CONSTRAINT wager_transactions_provider_idempotency_key_key
        UNIQUE (provider_id, idempotency_key),
    CONSTRAINT wager_transactions_provider_external_transaction_id_key
        UNIQUE (provider_id, external_transaction_id),

    -- Every opaque external identifier is bounded the same way: non-empty,
    -- at most 255 bytes. A NULL value (always true for INTERNAL rows)
    -- satisfies these trivially, since a CHECK only fails on an explicit
    -- FALSE, never on NULL/unknown.
    CONSTRAINT wager_transactions_external_transaction_id_len_check
        CHECK (octet_length(external_transaction_id) BETWEEN 1 AND 255),
    CONSTRAINT wager_transactions_provider_id_len_check
        CHECK (octet_length(provider_id) BETWEEN 1 AND 255),
    CONSTRAINT wager_transactions_idempotency_key_len_check
        CHECK (octet_length(idempotency_key) BETWEEN 1 AND 255),
    CONSTRAINT wager_transactions_round_id_len_check
        CHECK (octet_length(round_id) BETWEEN 1 AND 255),
    CONSTRAINT wager_transactions_game_id_len_check
        CHECK (octet_length(game_id) BETWEEN 1 AND 255),
    CONSTRAINT wager_transactions_reference_external_transaction_id_len_check
        CHECK (octet_length(reference_external_transaction_id) BETWEEN 1 AND 255)
);

-- Partial: OPENING is the only kind INTERNAL ever produces, and a wallet's
-- initial credit must exist at most once. Scoping to status = 'PROCESSED',
-- rather than every OPENING row regardless of status, is what "duplicar
-- crédito inicial" actually means - two OPENING rows that both succeeded -
-- while still allowing a wallet to be retried if some earlier attempt had
-- somehow ended REJECTED or FAILED instead.
CREATE UNIQUE INDEX wager_transactions_opening_per_wallet_idx
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING' AND status = 'PROCESSED';

-- Partial: this is the entire enforcement behind "uma BET aceita uma única
-- reversão bem-sucedida, REFUND ou ROLLBACK, nunca as duas". It keys off
-- reference_transaction_id - the resolved internal id, set once the
-- reference has actually been found - not the provider-supplied external
-- id, and only counts a reversal once it has PROCESSED. A REJECTED second
-- REFUND or ROLLBACK for the same reference does not collide here, which
-- is exactly the valid path the spec calls out ("uma REJECTED passa").
CREATE UNIQUE INDEX wager_transactions_reversal_per_reference_idx
    ON wager_transactions (reference_transaction_id)
    WHERE kind IN ('REFUND', 'ROLLBACK') AND status = 'PROCESSED';

GRANT SELECT, INSERT, UPDATE ON wager_transactions TO wallet_app;

-- The domain's state machine treats PENDING, PROCESSED, REJECTED and
-- FAILED such that PROCESSED, REJECTED and FAILED are terminal - any
-- transition out of them is a domain error - and this trigger is the
-- database's own copy of that rule. It has to hold even for a raw UPDATE
-- issued through wallet_app's own grants, which necessarily allow UPDATE
-- in the first place so a PENDING_REFERENCE row can resolve into one of
-- the three terminal states.
CREATE FUNCTION wager_transactions_block_terminal_update() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'wager_transactions: row % is terminal (%) and cannot be modified', OLD.id, OLD.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wager_transactions_block_terminal_update
    BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_block_terminal_update();
