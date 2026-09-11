# ClaimOps — Build Plan

## Product boundary
ClaimOps is an internal health-insurance claims back-office cognitive operations system.

It is NOT:
- a consumer insurance app,
- a diagnosis system,
- a medical-necessity engine,
- an autonomous claim approval/rejection system,
- a fraud adjudication engine,
- a payment system.

## Architecture
Deterministic control plane:
admission -> integrity -> parsing -> extraction -> evidence -> validation -> state/control

Cognitive layer:
exception investigation -> evidence retrieval -> hypothesis -> finding -> recommendation

Human/control layer:
HITL decision -> deterministic workflow transition -> verification -> audit

## Current roadmap

### Phase 1 — Foundation
- Go/Fiber API foundation
- strict contracts
- tenant context
- request IDs
- structured errors
- baseline observability

Status: COMPLETE

### Phase 2 — Deterministic core
- domain entities
- paise Money
- claim state machine
- validators
- tenant store
- versioning
- transition events

Status: COMPLETE

### Phase 3 — Persistence/security
- PostgreSQL
- RLS/FORCE RLS
- least privilege
- audit append-only rules
- concurrency/idempotency tests

Status: COMPLETE

### Phase 4 — External boundaries
- policy/TPA/provider contracts
- Mockoon mocks
- tenant enforcement
- contract tests

Status: COMPLETE

### Phase 5 — Document trust boundary
- admission
- MIME/sniff/filename checks
- SHA-256 integrity
- bounded reads
- transient vs terminal delivery
- failed cleanup observability

Status: COMPLETE

### Phase 6 — GCP transport/storage
- Pub/Sub
- outbox
- GCS
- DLQ
- localgcp E2E
- Cloud Run/Terraform deployment slices

Status: COMPLETE

### Phase 7 — Parser evaluation
- #28 canonical parser contract — COMPLETE
- #29 corpus + goldens — COMPLETE
- #30 LiteParse adapter — COMPLETE
- #31 deterministic benchmark harness — COMPLETE
- #32 benchmark comparison report — COMPLETE (docs/evaluations/parser/v1-report.md)
- #33 parser/OCR/routing ADR — COMPLETE (ADR-007: LiteParse default, sufficiency gate, OCR optional)

Status: COMPLETE

### Phase 8 — Deterministic claim processing
After parser decision (ADR-007):
- document classification
- extraction (#44/#45 — COMPLETE)
- evidence persistence
- cross-document consistency / canonical assembly (#46 — COMPLETE)
- deterministic reconciliation / verification R1–R10 + Unresolved channel (#47 — COMPLETE)
- exception generation (verify exceptions live; insufficient-parse exception path — PENDING)
- sufficiency-gate rules per document class (ADR-007 follow-up) — PENDING

### Phase 9 — Cognitive investigation
Introduce bounded Claim Investigation Agent only after deterministic exception generation is stable.

Agent capabilities should be explicit:
- get_claim
- get_policy_context
- get_documents
- get_evidence
- search_evidence
- get_verification_findings
- get_external_policy_status
- get_tpa_case
- create_investigation_report

Agent prohibitions:
- approve
- deny
- pay
- change policy
- override deterministic rules
- mutate authoritative state directly

### Phase 10 — Evaluation/security/production hardening
- LLM evals
- adversarial document tests
- prompt-injection/document-instruction tests
- PII/PHI leakage tests
- tenant isolation
- authorization
- fault injection
- replay/idempotency
- observability
- deployment validation

## Development sequence
Issue -> inspect repository/ADR/contracts -> plan -> TDD -> smallest implementation -> tests -> static checks -> integration/E2E -> diff review -> atomic commit -> PR -> merge -> issue closure.

Never skip the contract/evidence stage to reach an AI demo faster.
