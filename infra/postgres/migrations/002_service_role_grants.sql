-- 002_service_role_grants.sql — tighten the service role to least privilege.
--
-- Finding (Phase 3 integration probe): REVOKE ... FROM PUBLIC does not
-- remove the explicit GRANT this project gives claimops_app, so plain
-- UPDATE on audit_log succeeded for the service role. Append-only tables
-- must be SELECT+INSERT only for the service role.
--
-- Design note (documented divergence): claims.id stays a GLOBAL primary
-- key (claim IDs are unguessable UUIDs in practice). The in-memory store
-- keys by composite (tenant, id) and would hold two same-ID rows; SQL
-- instead rejects the cross-tenant insert (PK/RLS conflict surfaced as
-- ErrVersionConflict). Both behaviors are safe — no tenant can observe or
-- mutate another tenant's row — but they differ in response shape. If
-- same-ID coexistence is ever required, promote to PRIMARY KEY
-- (tenant_id, id) with composite foreign keys.

REVOKE UPDATE, DELETE ON claim_events FROM claimops_app;
REVOKE UPDATE, DELETE ON audit_log FROM claimops_app;
REVOKE UPDATE, DELETE ON claim_events FROM PUBLIC;
REVOKE UPDATE, DELETE ON audit_log FROM PUBLIC;

-- Belt and braces for future objects: service role never gains
-- UPDATE/DELETE by default; explicit per-table grants remain the norm.
ALTER DEFAULT PRIVILEGES FOR ROLE claimops
    IN SCHEMA public
    REVOKE UPDATE, DELETE ON TABLES FROM claimops_app;
