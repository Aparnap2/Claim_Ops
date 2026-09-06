-- 004_documents.sql — document intelligence foundation (branch feat/document-intelligence-foundation).
--
-- Contract (sibling Go agent builds against these exact names — do not rename):
--   documents, field_evidence
--   * string (TEXT) IDs, sha256 hex content hash, status strings
--   * documents: UNIQUE (tenant_id, claim_id, sha256)
--   * field_evidence: UNIQUE (tenant_id, claim_id, document_id, field)
--   * tenant isolation via FORCE RLS on tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
--   * documents + field_evidence are append-only for the service role (SELECT + INSERT only, no UPDATE/DELETE)
-- Design note (documented divergence): documents.status is logically mutable
--   (uploaded -> parsed -> failed), but this phase keeps the table append-only:
--   no UPDATE/DELETE for claimops_app. Status transitions create new rows
--   (superseding versions) or are handled in app memory. If in-place status
--   mutation is ever required, grant a narrow UPDATE (status) via policy/trigger,
--   never full UPDATE.
-- Idempotent: safe to apply more than once (IF NOT EXISTS / DROP POLICY IF EXISTS).
-- Note: documents predates this migration (001_init.sql created
--   id/tenant_id/claim_id/sha256/storage_uri). This migration is additive only:
--   it preserves storage_uri where it exists and adds the document-intelligence
--   columns. Fresh databases get the task shape from CREATE TABLE below.

CREATE TABLE IF NOT EXISTS documents (
  id          TEXT PRIMARY KEY,
  tenant_id   TEXT NOT NULL,
  claim_id    TEXT NOT NULL REFERENCES claims (id),
  type        TEXT NOT NULL,
  file_name   TEXT NOT NULL,
  mime        TEXT NOT NULL,
  sha256      TEXT NOT NULL,
  size_bytes  BIGINT NOT NULL CHECK (size_bytes >= 0),
  status      TEXT NOT NULL,
  created_at  TIMESTAMPTZ DEFAULT now(),
  UNIQUE (tenant_id, claim_id, sha256)
);

-- Additive evolution for databases where documents already exists (001_init.sql shape).
ALTER TABLE documents ADD COLUMN IF NOT EXISTS type TEXT NOT NULL;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS file_name TEXT NOT NULL;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS mime TEXT NOT NULL;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS sha256 TEXT NOT NULL;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0);
ALTER TABLE documents ADD COLUMN IF NOT EXISTS status TEXT NOT NULL;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ DEFAULT now();

CREATE TABLE IF NOT EXISTS field_evidence (
  id          TEXT PRIMARY KEY,
  tenant_id   TEXT NOT NULL,
  claim_id    TEXT NOT NULL,
  document_id TEXT NOT NULL REFERENCES documents (id),
  field       TEXT NOT NULL,
  value       TEXT NOT NULL,
  page        INT NOT NULL CHECK (page >= 1),
  anchor      TEXT NOT NULL DEFAULT '',
  confidence  DOUBLE PRECISION NOT NULL CHECK (confidence BETWEEN 0 AND 1),
  extractor   TEXT NOT NULL,
  created_at  TIMESTAMPTZ DEFAULT now(),
  UNIQUE (tenant_id, claim_id, document_id, field)
);

-- Row-level security: tenant isolation, enforced even for owners.
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents FORCE ROW LEVEL SECURITY;
ALTER TABLE field_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE field_evidence FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON documents;
CREATE POLICY tenant_isolation ON documents
  FOR ALL
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON field_evidence;
CREATE POLICY tenant_isolation ON field_evidence
  FOR ALL
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Append-only documents + field_evidence: service role reads and inserts; no UPDATE or DELETE.
REVOKE UPDATE, DELETE ON documents FROM PUBLIC;
REVOKE UPDATE, DELETE ON documents FROM claimops_app;
GRANT SELECT, INSERT ON documents TO claimops_app;
REVOKE UPDATE, DELETE ON field_evidence FROM PUBLIC;
REVOKE UPDATE, DELETE ON field_evidence FROM claimops_app;
GRANT SELECT, INSERT ON field_evidence TO claimops_app;
