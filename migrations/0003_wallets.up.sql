CREATE TABLE wallets (
    id         UUID NOT NULL CONSTRAINT wallets_id_pk PRIMARY KEY,
    player_id  UUID NOT NULL,
    currency   CHAR(3) NOT NULL,
    balance    BIGINT NOT NULL DEFAULT 0,
    version    BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- balance >= 0 is the bank's own line against a negative balance: even
    -- raw SQL through wallet_app's own grants, or a bug that skips the
    -- domain's Debit() check, cannot get a row past this constraint.
    -- version >= 1 matches Wallet.New/Rehydrate in the domain, which never
    -- produces a wallet below version 1; the optimistic-concurrency UPDATE
    -- the concurrency design relies on ("UPDATE da carteira com WHERE
    -- version = <lida>") depends on version only ever moving forward from
    -- there.
    CONSTRAINT wallets_balance_check CHECK (balance >= 0),
    CONSTRAINT wallets_version_check CHECK (version >= 1),

    -- What "a second wallet for the same player and currency" collides
    -- against, independently of the SELECT ... FOR UPDATE the application
    -- takes before it would otherwise find out.
    CONSTRAINT wallets_player_id_currency_key UNIQUE (player_id, currency)
);

GRANT SELECT, INSERT, UPDATE ON wallets TO wallet_app;
