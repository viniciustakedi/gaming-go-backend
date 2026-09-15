-- The migration owner is whichever role runs this file - locally and in
-- Docker Compose, POSTGRES_USER (see docker-compose.yml and README.md).
-- That role already owns every table the migrations below create, so it is
-- "o papel dono das migrations" the spec asks for; no separate role has to
-- be created for it.
--
-- wallet_app is the role the running service, and every seam 2 test in
-- this ticket, connects as. Every later migration in this series grants it
-- only the verbs the spec allows per table - most tellingly SELECT and
-- INSERT on the ledger, never UPDATE, DELETE or TRUNCATE, so the append-only
-- audit trail cannot be edited even by application code gone rogue, or SQL
-- injection through the app's own connection.
--
-- wallet_app itself is NOT created here. The role and its password are
-- cluster objects - shared with every other database in the same Postgres
-- instance, and outliving any single migration cycle - so they belong to
-- provisioning (deploy/postgres/provision.sh), never to a migration this
-- file's own down migration would otherwise have to undo. A `migrate down`
-- followed by `migrate up` must never touch the role or its password: this
-- migration only grants the schema-level privileges wallet_app needs, and
-- assumes the role already exists.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wallet_app') THEN
        RAISE EXCEPTION 'papel wallet_app ausente: rode o provisionamento do Postgres antes das migrations';
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO wallet_app;
