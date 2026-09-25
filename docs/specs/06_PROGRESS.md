# ClaimOps — Living Progress Tracker

Last updated: 2026-09-25

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
- [x] #44/#45 deterministic extraction (DocumentFacts, no-confidence rule)
- [x] #46 canonical assembly (agreement, ConflictEntry/NeedsReview, typed-mapping gate)
- [x] #47 verification R1–R10 + verifywrap Unresolved channel
- [x] #50 Agreed ordering stability (derive Agreed from sorted sources; 604936e, PR #55)
- [x] #53 investigation contracts (exception envelope, epistemics, capabilities; f85cf79, PR #62)
- [x] #54 bounded evidence/tool capabilities (11 tools; 229ebd6, PR #64)
- [x] #66 bounded investigation orchestrator (FakeModelClient-first; 877c052, PR #68)
- [x] eval v1 bounded-investigation harness + baseline (E0 gates, A–P corpus; ca98d96, PR #71; PR #73)
- [x] #80 Phase-3 slices 1–2: GCW provider + Agent service + Mock/Groq ModelClients (5cb15f1, PR #80)
- [x] APA-9 mandatory HMAC webhook auth + trusted tenant binding (1ad00dd, PR #82)
- [x] APA-10 canonical HITL pending-state persistence/query (f16e508, PR #81)
- [x] APA-11 authoritative mutation boundary + ADR-008 (10d8256, PR #83)
- [x] APA-12 typed OCRConfidence + low-evidence HITL gate + ADR-009 (5450ee5, PR #84)
- [x] APA-13 bounded retry classification, fallback, worker deadline (9f7a196, PR #85)
- [x] S3 explicit worker retry contract — preempt, raw ctx, DB class 23 terminal (1c3624f, PR #89)
- [x] F9 cross-layer retry composition bound 16 — S6 launch dedupe, no worker×workflow multiplication (34e3c8e, PR #92, APA-27)
- [x] P2 tenant-swap redelivery fail-closed — same-run TERMINAL rejection, post-restart TRANSIENT via durable boundary (6404ce2, PR #93, APA-28)
- [x] APA-30 pull-loop TRANSIENT Nack via app.PullCallback — push/pull parity (2152ad0, PR #94)
- [x] Go 1.27 toolchain migration
- [x] act-compatible CI (fast/integration/security, Layer A green under act)

## APA-8 qualification matrix linkage

- Canonical APA-8 qualification matrix lives on Linear APA-8 (comment c13658aa). This file records milestone status only; the Linear comment is authoritative for the matrix.

## Current

- [ ] Phase-3 E2E remainder (slices 3+ per docs/specs/10_PHASE3_E2E_QUALIFICATION.md; slices 1–2 landed #80 main@5cb15f1)
- [ ] Groq qualification on frozen harness (Fake vs Groq, hard gates fabricated=0 crossTenant=0 unauthorized=0; no eval mutation)

## Next

- [ ] document classification + evidence persistence (Phase 8 remainder)
- [ ] bounded cognitive investigation (specification only; no implementation yet)
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
