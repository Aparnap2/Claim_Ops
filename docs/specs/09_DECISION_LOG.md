# ClaimOps — Decision Log

## Purpose
This is a compact index of architectural decisions and important implementation choices. Detailed ADRs remain authoritative where they exist.

## Current decisions

### Deterministic-first architecture
Deterministic software owns truth, state, control, validation, and side effects. AI is bounded to ambiguity/cognition.

Reason:
- easier testing
- reproducibility
- fault localization
- safer automation
- narrower AI evaluation surface

### Go authoritative backend
Go/Fiber is the authoritative backend/system-of-record boundary.

Reason:
- strong typing
- concurrency/runtime suitability
- explicit contracts
- operational simplicity

### Python/ADK bounded cognitive service
Python is reserved for cognitive/AI workloads and is not the system of record.

### PostgreSQL + RLS
PostgreSQL is authoritative with tenant isolation enforced through RLS/FORCE RLS.

### IDs-only events
Pub/Sub events contain identifiers and hashes rather than document contents.

Reason:
- small messages
- lower leakage risk
- stable event contracts
- document data remains behind controlled storage access

### Document trust boundary
Untrusted input must pass admission and integrity verification before parsing.

### Evidence-first AI
The future investigation agent reasons over structured evidence and verified findings, not arbitrary raw documents whenever avoidable.

### LiteParse-only parser for current stage
LiteParse is the current parser adapter.

Docling was deliberately dropped from #30 because its installation/model-download/operational footprint was impractical for the current environment.

This is not a permanent statement that Docling is inferior.

### OCR is an escalation capability
OCR is currently disabled in the LiteParse adapter.

Managed GCP OCR may be added later if benchmark evidence demonstrates the need and operational/cost/privacy constraints are acceptable.

Do not make free-tier availability an architectural dependency.

### Parser contract is vendor-neutral
`internal/parser` owns:
- TrustedDocument
- canonical artifacts
- parser interface
- parser error taxonomy
- conformance

Vendor types remain inside adapters.

### Corpus is synthetic/reproducible
The parser benchmark corpus contains no real patient PII/PHI.
Synthetic documents are the primary reproducible benchmark source.

### JSON v2
Go 1.27 was adopted.
`encoding/json/v2` was evaluated and rejected for byte-sensitive outbox serialization because marshaling was not byte-identical under the existing contract.

This does not prohibit future use in non-byte-sensitive contexts after explicit review.

### Deterministic claim chain #44–#47 (complete)
Extraction observation (#44/#45, no confidence floats) -> canonical assembly (#46, agreement/ConflictEntry/NeedsReview) -> verification R1–R10 + Unresolved channel via verifywrap (#47). Investigation input (UnresolvedException) is specified only; no agent implementation exists.

## Decided: #33 parser/OCR/routing policy (ADR-007, accepted 2026-09-11)
#33 selected the parser/OCR/routing policy using measured evidence from #31/#32: LiteParse 2.14.4 default (OCR off), deterministic sufficiency gate, managed OCR optional on measured evidence. Detail: docs/adr/007-parser-policy.md. The pre-decision options considered were:

Options considered (historical):
- LiteParse sufficient as-is
- LiteParse + managed OCR escalation
- additional parser/capability required
- corpus expansion required before production decision

The decision was evidence-driven.

### APA-9 mandatory HMAC webhook auth + trusted tenant binding (accepted 2026-09-22)
HMAC-SHA256 over method + path (claim binding) + tenant (tenant binding)
+ raw body; constant-time compare; fail-closed in every env (no APP_ENV
bypass); empty secret → 500 WEBHOOK_MISCONFIGURED. Single canonical
trimmed tenantID feeds MAC + RLS/lookup/mutation/audit; padded header
rejected 400; tenant is header-only (never default/body). Optimistic
concurrency via UPDATE WHERE id/tenant/version + RowsAffected (same-event
loser → replay:true, different-event → 409 VERSION_CONFLICT). No
secrets/payloads in logs. Source: `fix(api): mandatory HMAC webhook auth
+ trusted tenant binding (APA-9) (#82)` (1ad00dd, PR #82).
Regression: `TestDecision` 18/18 (13 boundary + SignerBindsExactTenantBytes
+ PaddedHeaderUsesCanonicalIdentity + 3 live PG integration); `TestHITL`
4/4 intact; eval-v1 unchanged.

### APA-10 canonical HITL pending-state persistence/query (accepted 2026-09-22)
`ListPendingHITL` consumes the single canonical `hitlPendingStatuses`
`[HITL, ACTION_PENDING]` via `WHERE status = ANY($1)` parameterized.
Invariant: one canonical definition → query consumes it → tests protect
it. Source: `fix(postgres): canonical HITL pending-state
persistence/query (APA-10) (#81)` (f16e508, PR #81).
Regression: `TestHITL` 4/4 PASS live PG :5433 (PendingCanonicalMapping,
AllStatesRoundTrip, ListPendingReturnsPendingOnly, TenantIsolation);
eval-v1 unchanged.

### APA-11 agent authoritative mutation boundary (ADR-008, accepted 2026-09-22)
Agents are read-only over authoritative state. Deterministic code owns
claim/document/HITL mutations via version-checked, tenant-scoped commands
(`Repository.SaveClaim`, `AppendEvent`, `InsertDocument`, `handlers/decision`
`UPDATE claims WHERE version`). The cognitive loop never mutates
authoritative state: `cmd/agent` registry wires only read tools
(`get_claim`, `get_documents`, `get_evidence`, `search_evidence`,
`get_verification_findings`); `orchestrate.Loop` denies
`create_investigation_report` (T11) as `INVALID_OUTPUT`/`ErrToolDenied`
before any `Executor.Execute`. Report persistence (`PGReportStore.Insert`)
is write-once, hash-verified, envelope-bound, and reachable only via
deterministic service outside the loop. Detail: `docs/adr/008-agent-mutation-boundary.md`.
Regression: `orchestrate/mutation_boundary_test.go` (registry read-only,
loop T11 denial, report cannot carry `claim_status` transition).

### APA-12 OCR confidence semantics and low-evidence HITL gate (ADR-009, accepted 2026-09-22)
Typed `documents.OCRConfidence` (available vs unavailable, validated [0,1])
owns the trust policy; `IsLow(0.85)` is the HITL predicate
(unavailable/invalid or value <=0.85 -> HITL YES, value >0.85 -> HITL NO).
`Classify` 0.85 is deterministic classification, not provider OCR confidence
and never becomes available. LiteParse vendorSilent 1.0 is stamped ConfidenceAvailable=false → Unavailable → HITL; only blocks with ConfidenceAvailable=true may become available via NewOCRConfidence. Worker `runNewPipeline` gate after `Parse` uses
`aggregateOCRConfidence` -> `shouldEscalateForOCR` to route low-quality
documents to exception/HITL (`LOW_OCR_CONFIDENCE` + R8 envelope). Detail:
`docs/adr/009-ocr-confidence-hitl.md`. Regression: `documents/ocr_confidence_test.go`
(7 boundary cases) + `worker/processor_ocr_test.go` (8 unit + full-chain HITL integration).

### APA-13 bounded retry classification, fallback, worker deadline (accepted 2026-09-23)
Groq: 408/429/5xx retryable (1 retry + 10ms backoff), 4xx terminal
`ErrModelContract`, empty `ErrModelEmpty`, raw cancellation,
`: exhausted` marker. Loop: `isLoopRetryable` (retryable Upstream only) +
backoff; turn semantics unchanged. `FallbackModelClient` shared budget 4
with `markExhausted` terminal marker (Loop-level total exactly 4, not 8).
Worker: min(parent, now+60s) deadline envelope with raw ctx propagation.
Source: `feat(llm): bounded retry classification, fallback, worker
deadline (APA-13) (#85)` (9f7a196, PR #85).
Regression: 4 RED suites (Groq 4, Loop 5 groups, Worker 7, Fallback 9
incl. Loop-level bound) GREEN; filtered 37 PASS; eval-v1 PASS;
pytest 24 PASS; ruff PASS.

### S3 explicit worker retry contract (accepted 2026-09-23)
Frozen delivery contract for `internal/worker`: TRANSIENT retries while
budget permits (Tier-1 `MaxAttempts<=3`, new-pipeline Attempts==1, no
worker sleep — transport owns backoff); TERMINAL writes FAILED row
best-effort, ACK, never retry (including pgconn class 23 via
`retryableStoreErr`); CANCELLED returns raw `ctx.Err()` as TRANSIENT with
zero further attempts (`preempt` / `abortedByCtx`); DUPLICATE returns the
stored outcome with zero side effects (TRANSIENT is never remembered).
F2 mid-call hung dependencies stay out of scope (60s wall-clock envelope
bounds them; preemption would orphan writes). F3/F4 convergence is proven
against a dedup-mimicking store (UNIQUE + ON CONFLICT DO NOTHING; no
prod schema change). F8 classification applies at insert/load/check seams
only — list-classification remains a non-goal. Source: `feat(worker):
explicit retry contract — preempt, raw ctx, DB class 23 terminal (S3)
(#89)` (1c3624f, PR #89). Regression: `retry_contract_test.go` 15/15
(F2/F3/F4/F7/F8/F9 + interaction; 7 RED pre-fix).

### F9 cross-layer retry composition bound 16 (accepted 2026-09-24, APA-27)
Declared product 48 (3 worker × 4 workflow × 4 provider) was unreachable:
S6 durable launch/idempotency boundary prevents worker redelivery from
multiplying workflow executions (stable tenant-hashed investigation IDs,
persist-first + `GetLaunch` convergence, first-wins `RecordLaunch`,
Launcher performs no retries). Honest bound: 1 execution × 4 workflow
attempts × 4 provider calls = 16 per workflow step, proven tight via
real-Launcher redelivery test + real-fallback-seam composed test, both
red-proofed. Leaf package `internal/retrybudget/` only; no retry
semantics changed. Source: PR #92 (34e3c8e). Note: workflow portion is a
deterministic harness of workflow retry semantics, not live GCP E2E.

### P2 tenant-swap redelivery fail-closed (accepted 2026-09-24, APA-28)
RED exposed real cross-tenant adoption: tenant-B redelivery of
tenant-A-decided doc returned DUPLICATE with A's execution (processed-set
keyed by doc ID only; attrs tenant silently overwritten by bytes tenant).
Fix at tenant-binding seam only: same-run mismatch → TERMINAL REJECTION
(permanent, never SUCCESS/DUPLICATE, ACKs to avoid poison-loop);
post-restart fetch-miss → TRANSIENT per frozen S3 (TERMINAL would poison
genuine ingest races), bounded by Tier-1 attempts → transport redelivery
→ DLQ, no false ACK (pull Nack, push 503). Durable authority (option B):
tenant-scoped blob fetch, RLS/FORCE RLS, tenant-hashed investigation IDs,
fetch-before-side-effects. Migration/backfill: NONE (processed-set is
in-memory). Source: PR #93 (6404ce2).

### APA-30 pull-loop TRANSIENT Nack (accepted 2026-09-25)
`cmd/api` pull loop swallowed TRANSIENT (always-Ack); now propagates the
handler error (Nack) for TRANSIENT only via `app.PullCallback`, matching
push (503 → redeliver → DLQ). TERMINAL/SUCCESS/DUPLICATE still ACK
(poison-message protection at the frozen classification layer). Wiring/
contract only; retry classification and P2 semantics untouched. Source:
PR #94 (2152ad0).

### ADR-002 model string — open discrepancy (adjudication pending, recorded 2026-09-23)
`README.md:14` contracts Groq model `openai/gpt-oss-20b` per ADR-002 while
`apps/api/internal/investigate/orchestrate/groq_model.go:19` defaults to
`llama-3.1-8b-instant`. Code is unchanged by this docs pass. Explicit
options, no silent pick:
- Option A: amend ADR-002 (and README) to the code default.
- Option B: change the code default to the ADR-002 string.
Adjudication requires a code-owner decision; this entry records the
discrepancy only.

### #32 evidence (LiteParse 2.14.4, 45 cases, deterministic)
- 45/45 parse_ok. Fields 280 exact / 27 normalized / 150 missing. Tables 27 pass / 13 partial / 3 missed.
- D0 strong; D6/D7 collapse with EMPTY_ARTIFACT (OCR off by design).
- Table cells carry no block-id by contract (unavailable-by-design, not parser failure).
- Vendor-silent confidence 1.0 is uncalibrated; never rewarded by the scorer.
- Full evidence: docs/evaluations/parser/v1-report.md. Input to #33 recorded as questions, not decisions.
