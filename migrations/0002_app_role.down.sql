-- Only revokes what this migration's own up granted - the role and its
-- password are cluster objects owned by provisioning
-- (deploy/postgres/provision.sh), never by a migration, so this down never
-- drops or touches wallet_app itself. A `migrate down` followed by
-- `migrate up` therefore leaves the role, and its password, exactly as
-- provisioning left them.
REVOKE USAGE ON SCHEMA public FROM wallet_app;
