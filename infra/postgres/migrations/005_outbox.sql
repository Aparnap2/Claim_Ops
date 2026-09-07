-- 005_outbox.sql — transactional outbox for transport (branch feat/transport-outbox).
--
-- Contract (sibling Go agents code against these exact names — do not rename):
--   outbox_events
--   * event_id TEXT PRIMARY KEY (idempotency key; duplicate INSERT swallows 23505)
--   * tenant_id TEXT NOT NULL, aggregate_type TEXT NOT NULL, aggregate_id TEXT NOT NULL
--   * event_type TEXT NOT NULL, event_version TEXT NOT NULL
--   * payload JSONB NOT NULL, occurred_at TIMESTAMPTZ NOT NULL
--   * created_at TIMESTAMPTZ DEFAULT now()
--   * published_at TIMESTAMPTZ NULL (NULL = unpublished)
--   * publish_attempts INT NOT NULL DEFAULT 0
--   * last_error TEXT NOT NULL DEFAULT ''
--   * next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now()
--   * tenant isolation via FORCE RLS on tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
--   * SELECT + INSERT + UPDATE for the service role; no DELETE.
--   * UPDATE scope (by convention, enforced in code not grants): only
--   * published_at / publish_attempts / last_error / next_attempt_at are ever
--   * updated (claim -> publish-or-backoff). No row mutation otherwise:
--   * event_id, tenant_id, aggregate_*, event_*, payload, occurred_at and
--   * created_at are immutable after insert.
-- Idempotent: safe to apply more than once (IF NOT EXISTS / DROP POLICY IF EXISTS).

CREATE TABLE IF NOT EXISTS outbox_events (
  event_id         TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL,
  aggregate_type   TEXT NOT NULL,
  aggregate_id     TEXT NOT NULL,
  event_type       TEXT NOT NULL,
  event_version    TEXT NOT NULL,
  payload          JSONB NOT NULL,
  occurred_at      TIMESTAMPTZ NOT NULL,
  created_at       TIMESTAMPTZ DEFAULT now(),
  published_at     TIMESTAMPTZ NULL,
  publish_attempts INT NOT NULL DEFAULT 0,
  last_error       TEXT NOT NULL DEFAULT '',
  next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Row-level security: tenant isolation, enforced even for owners.
ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON outbox_events;
CREATE POLICY tenant_isolation ON outbox_events
  FOR ALL
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Outbox lifecycle needs UPDATE (claim -> mark-published / mark-failed with
-- backoff). Only published_at / publish_attempts / last_error /
-- next_attempt_at are ever updated — no row mutation otherwise. No DELETE:
-- published rows age out via a future retention job, never via the app role.
REVOKE DELETE ON outbox_events FROM PUBLIC;
REVOKE DELETE ON outbox_events FROM claimops_app;
GRANT SELECT, INSERT, UPDATE ON outbox_events TO claimops_app;
