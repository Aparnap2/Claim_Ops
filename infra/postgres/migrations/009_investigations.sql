-- 009_investigations.sql — durable investigation envelope store (Phase 3).
--
-- Contract:
--   investigations
--   * id TEXT PRIMARY KEY is the investigation_id (inv-...), so PRIMARY KEY enforces uniqueness
--   * tenant_id, claim_id, envelope JSONB (canonical invest.UnresolvedException)
--   * tenant isolation via FORCE RLS on tenant_id = current_setting('app.tenant_id', true)
--   * append-only for service role (SELECT + INSERT only)
-- Idempotent: safe to apply more than once.

CREATE TABLE IF NOT EXISTS investigations (
  id         TEXT PRIMARY KEY,
  tenant_id  TEXT NOT NULL,
  claim_id   TEXT NOT NULL,
  envelope   JSONB NOT NULL,
  created_at TIMESTAMPTZ DEFAULT now()
);

ALTER TABLE investigations ENABLE ROW LEVEL SECURITY;
ALTER TABLE investigations FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_iso ON investigations;
CREATE POLICY tenant_iso ON investigations
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

REVOKE UPDATE, DELETE ON investigations FROM PUBLIC;
REVOKE UPDATE, DELETE ON investigations FROM claimops_app;
GRANT SELECT, INSERT ON investigations TO claimops_app;
