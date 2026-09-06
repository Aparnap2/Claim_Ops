-- 001_init.sql — ClaimOps Phase 1 baseline. UUID PKs, NUMERIC(12,2), RLS tenant_iso.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- app.tenant_id set per-request (SET LOCAL app.tenant_id = '<uuid>'); empty => no rows.
CREATE OR REPLACE FUNCTION tenant_iso() RETURNS uuid
LANGUAGE sql STABLE AS $$ SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid $$;

CREATE TABLE tenants (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','SUSPENDED')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  email CITEXT UNIQUE,
  role TEXT NOT NULL CHECK (role IN ('ADMIN','EXAMINER','REVIEWER','AUDITOR')),
  status TEXT NOT NULL DEFAULT 'ACTIVE',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- email needs citext ext; fallback if missing:
-- CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE policies (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  policy_number TEXT NOT NULL,
  product_id TEXT NOT NULL,
  version INT NOT NULL DEFAULT 1,
  effective_from DATE NOT NULL,
  effective_to DATE,
  clauses JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, policy_number, version)
);

CREATE TABLE claims (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  claim_reference TEXT NOT NULL,
  policy_id UUID NOT NULL REFERENCES policies(id),
  member_id UUID NOT NULL,
  claim_type TEXT NOT NULL DEFAULT 'REIMBURSEMENT',
  status TEXT NOT NULL DEFAULT 'RECEIVED',
  claimed_amount NUMERIC(12,2) NOT NULL CHECK (claimed_amount >= 0),
  currency CHAR(3) NOT NULL DEFAULT 'INR',
  incident_date DATE,
  version INT NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, claim_reference)
);
CREATE INDEX ix_claims_tenant_policy ON claims(tenant_id, policy_id);

CREATE TABLE documents (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  claim_id UUID NOT NULL REFERENCES claims(id) ON DELETE CASCADE,
  document_type TEXT NOT NULL DEFAULT 'UNKNOWN'
    CHECK (document_type IN ('CLAIM_FORM','HOSPITAL_FINAL_BILL','DISCHARGE_SUMMARY','DIAGNOSTIC_REPORT','UNKNOWN')),
  storage_uri TEXT NOT NULL,
  sha256 CHAR(64) NOT NULL,
  mime_type TEXT NOT NULL DEFAULT 'application/pdf',
  status TEXT NOT NULL DEFAULT 'RECEIVED',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, claim_id, sha256)
);
CREATE INDEX ix_documents_claim ON documents(tenant_id, claim_id);

CREATE TABLE evidence (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  claim_id UUID NOT NULL REFERENCES claims(id) ON DELETE CASCADE,
  document_id UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  field_name TEXT NOT NULL,
  value TEXT NOT NULL,
  page INT NOT NULL DEFAULT 1 CHECK (page >= 1),
  source_span TEXT NOT NULL DEFAULT '',
  provenance JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_evidence_claim ON evidence(tenant_id, claim_id);
CREATE INDEX ix_evidence_doc ON evidence(tenant_id, document_id);

CREATE TABLE exceptions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  claim_id UUID NOT NULL REFERENCES claims(id) ON DELETE CASCADE,
  type TEXT NOT NULL CHECK (type IN ('MISSING_DOCUMENT','UNKNOWN_DOCUMENT','ENTITY_MISMATCH','DATE_CONFLICT','AMOUNT_CONFLICT','DUPLICATE_DOCUMENT','CONFLICTING_EVIDENCE','LOW_EXTRACTION_CONFIDENCE','POLICY_CONTEXT_MISSING')),
  severity TEXT NOT NULL DEFAULT 'MEDIUM' CHECK (severity IN ('LOW','MEDIUM','HIGH','BLOCKER')),
  status TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','INVESTIGATING','RESOLVED','ESCALATED')),
  detected_by TEXT NOT NULL DEFAULT 'VALIDATOR',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_exceptions_claim ON exceptions(tenant_id, claim_id) WHERE status IN ('OPEN','INVESTIGATING');

CREATE TABLE audit_events (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  claim_id UUID REFERENCES claims(id) ON DELETE SET NULL,
  actor_type TEXT NOT NULL,
  actor_id TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  entity TEXT NOT NULL,
  entity_id TEXT NOT NULL DEFAULT '',
  before_state JSONB, after_state JSONB,
  trace_id TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_audit_tenant_time ON audit_events(tenant_id, created_at DESC);

-- RLS: tenants table readable only to self; rest scoped to app.tenant_id
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_iso ON users         USING (tenant_id = tenant_iso()) WITH CHECK (tenant_id = tenant_iso());
CREATE POLICY tenant_iso ON policies      USING (tenant_id = tenant_iso()) WITH CHECK (tenant_id = tenant_iso());
CREATE POLICY tenant_iso ON claims        USING (tenant_id = tenant_iso()) WITH CHECK (tenant_id = tenant_iso());
CREATE POLICY tenant_iso ON documents     USING (tenant_id = tenant_iso()) WITH CHECK (tenant_id = tenant_iso());
CREATE POLICY tenant_iso ON evidence      USING (tenant_id = tenant_iso()) WITH CHECK (tenant_id = tenant_iso());
CREATE POLICY tenant_iso ON exceptions    USING (tenant_id = tenant_iso()) WITH CHECK (tenant_id = tenant_iso());
CREATE POLICY tenant_iso ON audit_events  USING (tenant_id = tenant_iso()) WITH CHECK (tenant_id = tenant_iso());
