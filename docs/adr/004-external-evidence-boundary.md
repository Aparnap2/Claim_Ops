# ADR-004: External claims-system integration and evidence boundary

Date: 2026-09-06
Status: accepted

## Context

The deterministic workflow must consume unreliable, legacy-style
external systems (policy admin, TPA claims, provider encounters, risk
signals) without letting their failure modes or shapes leak into the
domain. The future cognitive layer needs source-grounded evidence, not
raw upstream JSON.

## Decision

1. **Ports own the contract** (`internal/ports`): four read-only
   interfaces plus `ErrNotFound / ErrContract / ErrTenantMismatch /
   ErrUpstream`. `Retryable(err)` is true only for `ErrUpstream`.
   NHCX arrives later as another Port implementation; nothing here
   assumes a specific vendor shape.
2. **Adapters depend on ports** (`internal/adapters/http`): typed
   decode, required-key presence checks (truncation is `ErrContract`,
   never silent zero-fill), per-item tenant enforcement, request IDs,
   3s default timeouts. No retries inside adapters; retry lives in the
   workflow layer. Dependency direction Domain <- Workflow <- Ports <-
   Adapters is enforced by import discipline (domain imports nothing).
3. **Mocks are deterministic systems** (`mocks/mockoon`): one
   environment, four routes, scenario-keyed responses (happy, notfound,
   error, malformed, wrongtenant, partial, slow). Mockoon is test
   infrastructure, not a shipped dependency.
4. **Evidence is the bridge** (`internal/evidence` + `evidence` table):
   every materially influential external response becomes an
   `Evidence` row keyed `(tenant, claim, source_type, source_id)` with
   a content hash and RLS + append-only enforcement. Agent conclusions
   later cite evidence IDs, never raw payloads.
5. **Money stays paise-int64** across the boundary; dates are RFC3339.

## Consequences

- Upstream schema drift fails loudly at the edge as `ErrContract`.
- Tenant drift fails as `ErrTenantMismatch`, per item where lists
  are involved.
- Timeouts/5xx are the only retryable class; everything else routes
  to exceptions, never silent retries.
