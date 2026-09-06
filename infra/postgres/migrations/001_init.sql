-- 001_init.sql — ClaimOps tenant persistence baseline (branch feat/postgres-tenant-persistence).
--
-- Contract (sibling Go agent builds against these exact names — do not rename):
--   claims, claim_events, documents, audit_log, idempotency_keys
--   * string (TEXT) IDs, int64 paise, status strings, version int, DATE dates
--   * tenant isolation via FORCE RLS on tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
--   * claim_events + audit_log are append-only (REVOKE UPDATE, DELETE FROM PUBLIC)
-- Idempotent: safe to apply more than once (IF NOT EXISTS / DROP POLICY IF EXISTS).

CREATE TABLE IF NOT EXISTS claims (
  id            TEXT PRIMARY KEY,
  tenant_id     TEXT NOT NULL,
  policy_id     TEXT NOT NULL,
  reference     TEXT NOT NULL,
  amount_paise  BIGINT NOT NULL CHECK (amount_paise >= 0),
  status        TEXT NOT NULL,
  version       INT NOT NULL CHECK (version >= 1),
  incident_date DATE,
  admission_date DATE,
  discharge_date DATE,
  created_at    TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS claim_events (
  id          BIGSERIAL PRIMARY KEY,
  tenant_id   TEXT NOT NULL,
  claim_id    TEXT NOT NULL REFERENCES claims (id),
  seq         INT NOT NULL,
  type        TEXT NOT NULL,
  from_status TEXT NOT NULL,
  to_status   TEXT NOT NULL,
  event_id    TEXT NOT NULL,
  created_at  TIMESTAMPTZ DEFAULT now(),
  UNIQUE (tenant_id, claim_id, event_id)
);

CREATE TABLE IF NOT EXISTS documents (
  id          TEXT PRIMARY KEY,
  tenant_id   TEXT NOT NULL,
  claim_id    TEXT NOT NULL REFERENCES claims (id),
  sha256      TEXT NOT NULL,
  storage_uri TEXT NOT NULL,
  UNIQUE (tenant_id, claim_id, sha256)
);

CREATE TABLE IF NOT EXISTS audit_log (
  id         BIGSERIAL PRIMARY KEY,
  tenant_id  TEXT NOT NULL,
  claim_id   TEXT NOT NULL,
  actor_type TEXT NOT NULL,
  actor_id   TEXT NOT NULL,
  action     TEXT NOT NULL,
  entity     TEXT NOT NULL,
  entity_id  TEXT NOT NULL,
  "before"   JSONB NOT NULL DEFAULT '{}',
  "after"    JSONB NOT NULL DEFAULT '{}',
  trace_id   TEXT NOT NULL,
  created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
  tenant_id  TEXT NOT NULL,
  claim_id   TEXT NOT NULL,
  event_id   TEXT NOT NULL,
  created_at TIMESTAMPTZ DEFAULT now(),
  PRIMARY KEY (tenant_id, claim_id, event_id)
);

-- Row-level security: tenant isolation on every table, enforced even for owners.
ALTER TABLE claims ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims FORCE ROW LEVEL SECURITY;
ALTER TABLE claim_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE claim_events FORCE ROW LEVEL SECURITY;
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON claims;
CREATE POLICY tenant_isolation ON claims
  FOR ALL
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON claim_events;
CREATE POLICY tenant_isolation ON claim_events
  FOR ALL
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON documents;
CREATE POLICY tenant_isolation ON documents
  FOR ALL
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON audit_log;
CREATE POLICY tenant_isolation ON audit_log
  FOR ALL
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON idempotency_keys;
CREATE POLICY tenant_isolation ON idempotency_keys
  FOR ALL
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Append-only event/audit tables: no UPDATE or DELETE for PUBLIC.
REVOKE UPDATE, DELETE ON claim_events FROM PUBLIC;
REVOKE UPDATE, DELETE ON audit_log FROM PUBLIC;
