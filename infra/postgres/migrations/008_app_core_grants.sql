-- 008_app_core_grants.sql — least-privilege grants the service role needs
-- on the core tables from 001_init.sql.
--
-- Finding (localgcp Cloud SQL qualification): a fresh database built
-- from migrations 001-007 leaves claimops_app with NO privileges on
-- claims, claim_events, or audit_log (002 only REVOKEs; 003/004/005/007
-- grant their own tables). Local dev worked only via undocumented
-- manual grants (state drift). This migration closes the gap so any
-- fresh database — localgcp Cloud SQL, CI postgres, or prod Cloud SQL —
-- is usable from migrations alone.
--
-- Least privilege per table:
--   claims: SELECT + INSERT + UPDATE (upsert is INSERT ... ON CONFLICT
--     (id) DO UPDATE; no DELETE — claims are never deleted).
--   claim_events, audit_log: SELECT + INSERT (append-only; 002 already
--     revokes UPDATE/DELETE, belt and braces here too).
--   idempotency_keys: intentionally NO grant (dead schema, no code
--     reads/writes it as of this migration).

GRANT SELECT, INSERT, UPDATE ON claims TO claimops_app;
GRANT SELECT, INSERT ON claim_events TO claimops_app;
GRANT SELECT, INSERT ON audit_log TO claimops_app;

-- Serial sequences backing the append-only tables: INSERTs need USAGE
-- (nextval) on the sequence itself, a separate privilege from the table.
GRANT USAGE, SELECT ON SEQUENCE claim_events_id_seq TO claimops_app;
GRANT USAGE, SELECT ON SEQUENCE audit_log_id_seq TO claimops_app;

REVOKE UPDATE, DELETE ON claims FROM PUBLIC;
REVOKE UPDATE, DELETE ON claim_events FROM PUBLIC;
REVOKE UPDATE, DELETE ON audit_log FROM PUBLIC;
