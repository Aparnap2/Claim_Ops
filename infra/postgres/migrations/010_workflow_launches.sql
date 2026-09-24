-- 010_workflow_launches.sql — durable workflow-launch state (S6/APA-25).
--
-- Contract:
--   workflow_launches
--   * investigation_id TEXT PRIMARY KEY is the durable idempotency key:
--     PRIMARY KEY enforces first-write-wins (repeat RecordLaunch converges
--     via ON CONFLICT DO NOTHING, never duplicates a StartExecution)
--   * tenant_id, claim_id, workflow_id, execution_name, launched_at
--   * tenant isolation via FORCE RLS on tenant_id = current_setting('app.tenant_id', true)
--   * append-only for service role (SELECT + INSERT only): launch rows are
--     facts, never mutated (no UPDATE/DELETE for claimops_app or PUBLIC)
-- Idempotent: safe to apply more than once.

CREATE TABLE IF NOT EXISTS workflow_launches (
  investigation_id TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL,
  claim_id         TEXT NOT NULL,
  workflow_id      TEXT NOT NULL,
  execution_name   TEXT NOT NULL,
  launched_at      TIMESTAMPTZ DEFAULT now()
);

ALTER TABLE workflow_launches ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_launches FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_iso ON workflow_launches;
CREATE POLICY tenant_iso ON workflow_launches
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

REVOKE UPDATE, DELETE ON workflow_launches FROM PUBLIC;
REVOKE UPDATE, DELETE ON workflow_launches FROM claimops_app;
GRANT SELECT, INSERT ON workflow_launches TO claimops_app;
