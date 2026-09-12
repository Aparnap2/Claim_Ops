-- 007_investigation_reports.sql — investigation report write-once store (issue #54 Chunk A).
--
-- Contract (sibling Go agents build against these exact names — do not rename):
--   investigation_reports
--   * string (TEXT) IDs, sha256 hex report hash, version int, canonical JSONB report
--   * one row per investigation: id IS the investigation_id (inv-...), so the
--     PRIMARY KEY on id enforces UNIQUE(investigation_id) with no second
--     column that could diverge; inserts must set id = investigation_id
--   * tenant isolation via FORCE RLS on tenant_id = current_setting('app.tenant_id', true)
--   * reports are append-only for the service role (SELECT + INSERT only, no UPDATE/DELETE)
-- Idempotent: safe to apply more than once (IF NOT EXISTS / DROP POLICY IF EXISTS).

CREATE TABLE IF NOT EXISTS investigation_reports (
  id            TEXT PRIMARY KEY,
  tenant_id     TEXT NOT NULL,
  claim_id      TEXT NOT NULL,
  exception_id  TEXT NOT NULL,
  report        JSONB NOT NULL,
  report_hash   TEXT NOT NULL,
  version       INT NOT NULL DEFAULT 1 CHECK (version >= 1),
  created_at    TIMESTAMPTZ DEFAULT now()
);

-- Row-level security: tenant isolation, enforced even for owners.
ALTER TABLE investigation_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE investigation_reports FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_iso ON investigation_reports;
CREATE POLICY tenant_iso ON investigation_reports
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Append-only reports: service role reads and inserts; no UPDATE or DELETE.
REVOKE UPDATE, DELETE ON investigation_reports FROM PUBLIC;
REVOKE UPDATE, DELETE ON investigation_reports FROM claimops_app;
GRANT SELECT, INSERT ON investigation_reports TO claimops_app;
