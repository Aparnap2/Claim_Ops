-- 006_worker_role.sql — dedicated least-privilege role for the outbox
-- dispatcher, which must claim rows across ALL tenants.
--
-- Why a separate role: outbox_events has FORCE RLS with a tenant check.
-- Tenant traffic connects as claimops_app (scoped by app.tenant_id GUC).
-- The dispatcher cannot set one tenant (it serves all of them), and any
-- GUC-based bypass readable by claimops_app would punch a hole in tenant
-- isolation. A distinct role with its own permissive policy keeps the
-- bypass explicit, auditable, and unusable from request paths.
--
-- claimops_worker must NEVER serve tenant-scoped request traffic.
-- Local auth: like claimops_app, this role needs a password provisioned
-- out-of-band (ALTER ROLE claimops_worker WITH PASSWORD '...') because
-- remote (non-loopback) connections fall through to scram-sha-256.
-- Production (Cloud SQL / terraform) must attach a strong password or IAM
-- auth — never rely on this migration's LOGIN without credentials outside
-- local dev.
-- Idempotent: DO-block role creation; DROP POLICY IF EXISTS.

DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'claimops_worker') THEN
    CREATE ROLE claimops_worker WITH LOGIN;
  END IF;
END
$$;

GRANT CONNECT ON DATABASE claimops TO claimops_worker;
GRANT USAGE ON SCHEMA public TO claimops_worker;
GRANT SELECT, INSERT, UPDATE ON outbox_events TO claimops_worker;

DROP POLICY IF EXISTS worker_all ON outbox_events;
CREATE POLICY worker_all ON outbox_events
  FOR ALL TO claimops_worker
  USING (true)
  WITH CHECK (true);
