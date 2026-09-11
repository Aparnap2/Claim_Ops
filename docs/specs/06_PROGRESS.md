# ClaimOps — Living Progress Tracker

Last updated: 2026-09-11

## Completed

- [x] Go/Fiber API foundation
- [x] Strict request validation
- [x] Request ID propagation
- [x] Tenant boundary
- [x] Deterministic claim state machine
- [x] Deterministic validators
- [x] PostgreSQL persistence
- [x] RLS + FORCE RLS
- [x] Least-privilege DB role
- [x] Optimistic concurrency/version guards
- [x] Append-only audit behavior
- [x] External insurance contracts
- [x] Mockoon contract matrix
- [x] Production engineering/observability standard
- [x] Deterministic document worker baseline
- [x] Pub/Sub adapter
- [x] Transactional outbox
- [x] GCS BlobStore
- [x] DLQ/server-side retry
- [x] Cloud Run/Terraform deployment slice
- [x] Document admission trust boundary
- [x] SHA-256 blob integrity verification
- [x] Transient/terminal delivery semantics
- [x] Failed cleanup observability
- [x] #28 canonical parser contract
- [x] #29 45-case synthetic corpus + goldens
- [x] #30 LiteParse-only parser adapter
- [x] #31 deterministic parser benchmark harness
- [x] #32 benchmark report generator + LiteParse v1 evidence
- [x] #33 parser policy ADR-007 (LiteParse default, sufficiency gate, OCR optional)
- [x] Go 1.27 toolchain migration
- [x] act-compatible CI (fast/integration/security, Layer A green under act)

## Current

- [ ] Sufficiency-gate rules per document class (follow-up of ADR-007)
- [ ] Exception path for insufficient parses in claim workflow

## Next

- [ ] deterministic claim extraction/verification workflow
- [ ] bounded cognitive investigation
- [ ] HITL recommendation workflow
- [ ] end-to-end evaluation
- [ ] production hardening

## Open engineering work

- [ ] #27 GCS orphan reconciliation/sweeper

## Explicit decisions

- [x] LiteParse selected as current parser implementation for the evaluation stage.
- [x] Docling dropped from current implementation due to operational/install/model-download overhead.
- [x] OCR is off in the current LiteParse adapter.
- [x] Managed GCP OCR is a future capability, not an MVP dependency.
- [x] `encoding/json/v2` rejected for byte-sensitive outbox serialization after byte-identity verification.
- [x] No real PII/PHI in repository corpus.
- [x] Synthetic corpus is the primary reproducible benchmark source.
- [x] Parser vendor types do not cross the application-owned parser boundary.

## Update rule
Every merged issue must update this file when it materially changes architecture, milestone status, or an explicit decision.

Do not mark work complete because code exists. Mark it complete only after the defined proof/tests have passed.
