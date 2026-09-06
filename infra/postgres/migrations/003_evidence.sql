-- 003_evidence.sql — external evidence capture baseline (branch feat/external-contracts).
--
-- Contract (sibling Go agent builds against these exact names — do not rename):
--   evidence
--   * string (TEXT) IDs, sha256 hex content hash, status strings
--   * one row per upstream fetch: UNIQUE (tenant_id, claim_id, source_type, source_id)
--   * tenant isolation via FORCE RLS on tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
--   * evidence is append-only for the service role (SELECT + INSERT only, no UPDATE/DELETE)
-- Idempotent: safe to apply more than once (IF NOT EXISTS / DROP POLICY IF EXISTS).

CREATE TABLE IF NOT EXISTS evidence (
  id           TEXT PRIMARY KEY,
  tenant_id    TEXT NOT NULL,
  claim_id     TEXT NOT NULL,
  source_type  TEXT NOT NULL,
  source_id    TEXT NOT NULL,
  retrieved_at TIMESTAMPTZ NOT NULL,
  content_hash TEXT NOT NULL,
  status       TEXT NOT NULL,
  UNIQUE (tenant_id, claim_id, source_type, source_id)
);

-- Row-level security: tenant isolation, enforced even for owners.
ALTER TABLE evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON evidence;
CREATE POLICY tenant_isolation ON evidence
  FOR ALL
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Append-only evidence: service role reads and inserts; no UPDATE or DELETE.
REVOKE UPDATE, DELETE ON evidence FROM PUBLIC;
REVOKE UPDATE, DELETE ON evidence FROM claimops_app;
GRANT SELECT, INSERT ON evidence TO claimops_app;
