# ClaimOps — Living Progress Tracker

Last updated: 2026-10-08
Current base: main@dffa160 (APA-64)

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
- [x] APA-31 per-class sufficiency gate — extract→Evaluate→assemble→verify, R8/MissingEvidence HITL routing (362c676, PR #96)
- [x] APA-33 docs backfill of the APA-31 gate into this file + decision log (803e564)
- [x] APA-34 Phase-3 slice A — MockModelClient → Agent HTTP → REPORT_READY with live PG proof, claim row unchanged (3d2d71f, PR #99)
- [x] APA-35 Phase-3 slice B — live GCW deploy + one execution driven to SUCCEEDED against emulator :8787, no workflow YAML change (ea7801f, PR #100)
- [x] APA-36 Phase-3 slice C — full `workflows/claim-investigation.yaml` live run with the real Agent backend (2e210e0, PR #101; exact branch result needs confirmation — see KL-01)
- [x] APA-37 GCW emulator step-level `retry:` conformance gate landed and staying red by design (PR #102; BLOCKED on upstream, KL-01/KL-10)
- [x] APA-38 cognitive failure matrix over real PGReaders — 8 service-level cases qualified; DEADLINE qualified at loop level only, because the service boundary has no deterministic delay seam (d05dd92, PR #103)
- [x] APA-39 worker→workflow correlation against real GCWProvider — second `EnsureLaunched` returns launched=false with no second `StartExecution` (24bb409, PR #104)
- [x] APA-40 GCWProvider honours `execution_name` as `executionId` + maps create-409 to create-or-return; 6 unit cases (79daf9d, PR #105; real-GCW adoption NOT locally qualified, KL-02)
- [x] APA-41 reconciler probes the full GCW resource name, not a bare execution id — bare name measured typed-absent 404 (5f86ed1, PR #106)
- [x] APA-42 `GetExecution` classifies resolved-but-FAILED as a state, not a lookup outage, so the reconciler adopts instead of failing closed (5633913 PR #107 live crash-window; 6ba22e4 PR #108)
- [x] APA-43 live infra failure matrix against real services (da7ff3d, PR #113; PR #109 reverted by ba1d2f5 and redone) — surfaced APA-44
- [x] APA-44 outbox publish attempts bounded so a dead Pub/Sub cannot stall dispatch (882889a, PR #112)
- [x] APA-45 Groq provider selection / model string / key handling, qualified offline with no live key under `unshare -rn` (c28775b, PR #111; its model-string claim is superseded, KL-14)
- [x] APA-47 Groq→Qwen model re-adjudication (`ad0d631`, PR #114) — Llama retired (HTTP 404), `qwen/qwen3.8-27b` confirmed live-servable. Identifier collides with the Linear APA-47 OCRProvider slice (KL-13)
- [x] APA-48 P0 worker parse runtime shipped into the worker image (48.3MB → 132MB, multi-stage); harness/env faults → `ErrRuntimeUnavailable` → existing `OutcomeTransient`, never a bad-document FAILED row (d47a73c, PR #115 → main@104d46c; deployment verification still outstanding, KL-03)
- [x] APA-49 P0 prompt/decoder field-name contract — `action` not `act`, snake_case tool wire schema, verbatim payload capture, report schema documented (ac73455, 5036c7f, 47990e8, ef6aa5d, PR #116)
- [x] APA-50 act-transition policy and no-repeat rule stated against observable history fields in the production prompt (3410521, 9a4e90e, PR #117)
- [x] APA-51 diagnostic: single-variable model swap shows the Qwen REPETITION attractor is model-specific (Backlog; diagnostic only, not a model decision and not a qualification)
- [x] APA-52 S1 harness corrected to real PGReaders with per-repetition accounting and mandatory tool execution (d02ff0e, PR #118)
- [x] APA-53 test isolation + ephemeral PostgreSQL qualification environment; fails closed if the app role is superuser or bypasses RLS (35107cf PR #119; 515fa22 + 3c2e2b2 PR #120)
- [x] APA-54 tool/knob ownership contract stated model-facing (3238ddf, PR #121)
- [x] APA-56 `missing_additive` kind↔key vocabulary stated model-facing, read from `invest.ExternalSourceKeys()` so prompt and vocabulary cannot drift; validator unchanged (d778ddc, PR #122)
- [x] APA-57 anti-fake-green LLM gate — a run that requested an LLM matrix and measured nothing is INFRA_BLOCKED with a non-zero exit, not QUALIFIED (c879e71, PR #123)
- [x] APA-58 APA-55 live cases proven non-distinct offline (six labels, one byte-identical prompt, no tool ever called) and replaced with per-scenario premise-checked fixtures (2424312 PR #124; 1c08705 PR #125)
- [x] APA-59 per-tool limit bounds clause: A/B harness + `qualify.sh` entry enforcement (8ddd416, PR #126); executor-observed measurement correction (b2b7c97, PR #127); clause shipped to the production prompt with fidelity pinned to sha256 `f57cf7ad58d360331e2cae9a914da3dea7df557751a41e2946667a3a1a1cdd5b` (6a308c6, PR #128)
- [x] APA-55 live qualification layer corrected — it was label-only (six scenarios, one byte-identical prompt) and now carries per-scenario envelopes, pre-call premise assertions, anti-vacuity, and a precise expected outcome per scenario (7a24cfd, PR #129)
- [x] APA-64 deadline/pacing interaction adjudicated deterministically with a fake model and injected pacing — establishes where DEADLINE lands and that pacing is charged to the investigation budget; nothing changed in the fixture, loop, deadline policy, prompt, or model (dffa160, PR #130)
- [x] Go 1.27 toolchain migration
- [x] act-compatible CI (fast/integration/security, Layer A green under act)

## APA-8 qualification matrix linkage

- Canonical APA-8 qualification matrix lives on Linear APA-8 (comment c13658aa). This file records milestone status only; the Linear comment is authoritative for the matrix.

## Live qualification evidence

This file records milestone status and points at where measurement lives.
It is not the measurement record. Run artefacts are not committed.

Identifier scope note: APA-60, APA-61 and APA-62 are **FinSight** work
(P10-01 data inventory, P10-02 sanitization, P10-03 LLM privacy boundary),
tracked on the FinSight repo. They are deliberately absent from every
ClaimOps list here. The ClaimOps numbering is otherwise contiguous from
APA-47 to APA-64.

### Final Qwen S1 — PASS (APA-63)

- Model `qwen/qwen3.8-27b`; 3/3 repetitions `REPORT_READY`, `actClass=VALID_ACT`, verdict `BOUNDARY_HELD`, 0 violations.
- Run on a confirmed-empty ephemeral PostgreSQL (0 rows across all public tables before the suite), via `infra/postgres/qualify.sh`, with the declared LLM matrix fully measured (`LLM cases measured: 1/1; skipped subtests: 0`, exit 0).
- Every repetition executed real tools against real PostgreSQL (`toolExecutions=2`), and `citationsFromTool == citations` on all three: every cited evidence ID originated from a real tool result.
- Production prompt fidelity pinned to sha256 `f57cf7ad…`; APA-59 confirmed on the production model by an independent probe returning `call_tool get_claim` with `"limit": 1`.
- **This result is recorded in Linear APA-63 and is NOT committed as a run artefact.** The repo holds the harness, not the log. Anyone re-asserting it must re-run or cite Linear; they cannot read it out of the tree.

### APA-55 live qualification — NOT qualified (APA-64)

- Last measured run was **INCOMPLETE: 1/5 scenarios measured.** Do not mark APA-55 passing.
- Control `ps_a1_control` measured 3/3 as `ESCALATED(DEADLINE)` with `toolExecutions=0`.
- The other four scenarios (`ps_a2_insufficient`, `ps_a4_fabricated`, `ps_a7_stale`, `ps_b1_valid_tool`) recorded **zero measurements** — provider throttled 3× each.
- `violations=0` on the measured control means nothing leaked. It is not a pass, and no claim about model behaviour on the four unmeasured scenarios exists.
- Two causes, neither a model failure: fixture `DeadlineMs = 60000` versus ≈60s harness pacing per model call (Cause 1, adjudicated by APA-64 as a harness/fixture interaction, not a production defect), and provider quota exhaustion (Cause 2, KL-04).

## Current

- [ ] APA-55 live re-run after the APA-64 deadline/pacing adjudication — the blocker for any further claim about model behaviour on insufficient / fabricated / stale / valid-tool scenarios
- [ ] Phase-3 E2E remainder per docs/specs/10_PHASE3_E2E_QUALIFICATION.md — slices 1–2 landed #80 (main@5cb15f1), slices A/B/C landed APA-34/35/36; the `check`-switch remainder (ESCALATED branch, HITL callback create/resume) is blocked on KL-01
- [ ] Fake-vs-Groq comparison on the frozen eval-v1 harness (fabricated=0, crossTenant=0, unauthorized=0) — manual live-key gate, not CI (KL-11)
- [x] Groq qualification on frozen harness — **narrowed**: the S1 scenario is qualified (APA-63, above). The broader happy-path-and-unhappy-path campaign is APA-55 and is NOT qualified
- [x] Bounded cognitive investigation — shipped, not specification-only: 11 read-only tools (#54), FakeModelClient-first orchestrator (#66), authoritative mutation boundary (APA-11/ADR-008), and a live-model S1 pass (APA-63). Previously listed under Next as "specification only; no implementation yet", which contradicted this file's own Completed list; removed from Next on that basis. Remaining qualification gaps are tracked as their own items above

## Next

- [ ] Release gate: production smoke after a qualifying APA-55 re-run
- [ ] Phase 8 remainder — **partially shipped, remainder honestly open.** Exception generation and the per-class sufficiency gate landed in APA-31 (PR #96). Not yet proven: a production-shaped exception path end-to-end, and the insufficient-parse exception path that `05_BUILD_PLAN.md` still lists as PENDING. Phase 8 is not complete and nothing here is claimed qualified
- [ ] APA-56 causal qualification against an envelope that actually carries `MissingEvidence` (KL-05)
- [ ] APA-37/APA-40 resumption once the upstream emulator lands (KL-01, KL-02, KL-10) — see Linear APA-46 for the ordered checklist
- [ ] Worker image rebuild/redeploy from the repo-root context, then one real production-like parse (KL-03)
- [ ] HITL recommendation workflow
- [ ] end-to-end evaluation
- [ ] production hardening

## Open engineering work

- [ ] #27 GCS orphan reconciliation/sweeper
- [ ] Retire the `action.go` ↔ `exception.go` four-key duplication; deliberate deferred debt from `d778ddc` (KL-08)
- [ ] Resolve the APA-47 identifier collision between the Groq→Qwen re-adjudication and the Linear OCRProvider slice (KL-13)
- [ ] Annotate Linear APA-45 as superseded on its model-string claim (KL-14)

## Explicit decisions

- [x] LiteParse selected as current parser implementation for the evaluation stage.
- [x] Docling dropped from current implementation due to operational/install/model-download overhead.
- [x] OCR is off in the current LiteParse adapter.
- [x] Managed GCP OCR is a future capability, not an MVP dependency.
- [x] `encoding/json/v2` rejected for byte-sensitive outbox serialization after byte-identity verification.
- [x] No real PII/PHI in repository corpus.
- [x] Synthetic corpus is the primary reproducible benchmark source.
- [x] Parser vendor types do not cross the application-owned parser boundary.
- [x] Groq production model is `qwen/qwen3.8-27b` (ADR-002 + code default + README + `.env.example` must all agree); `llama-3.1-8b-instant` is retired and superseded (ad0d631, PR #114)
- [x] A live run that exposes a model-facing gap is fixed in the prompt/contract, never by relaxing a deterministic validator or bound (APA-49, APA-50, APA-54, APA-56, APA-59)
- [x] Model-facing bounds are rendered from the authoritative constants (`investigate.MaxRows`, `invest.ExternalSourceKeys()`), never restated, with a divergence test pinning the two (APA-56, APA-59)
- [x] A qualification run that measures nothing is INFRA_BLOCKED with a non-zero exit, never QUALIFIED (APA-57; APA-59 trajectory gate at the same entry point)
- [x] Parse correctness outranks image size: the 132MB worker image is an accepted trade-off, never a reason to weaken the parser/evidence contract (APA-48)
- [x] Harness and environment faults are `ErrRuntimeUnavailable` routed to the existing `OutcomeTransient`; real document faults stay TERMINAL and still record the FAILED row (APA-48)
- [x] Qualification runs against an ephemeral, confirmed-empty PostgreSQL rather than the accumulating shared volume, and fail closed if the app role is superuser or bypasses RLS (APA-53)
- [x] Qualification run artefacts live in the issue tracker, not in the repo. The repo holds the harness; the ledger holds the measurement.

## Update rule
Every merged issue must update this file when it materially changes architecture, milestone status, or an explicit decision.

Do not mark work complete because code exists. Mark it complete only after the defined proof/tests have passed.

Rule history: this rule was violated across APA-47..APA-64 and PRs #126–#130 — this file sat at APA-31 while the repository advanced twenty merges. It was backfilled on 2026-10-08, and a limitation register (`docs/KNOWN_LIMITATIONS.md`) now exists so that the next gap is recorded where a reader will look for it rather than in an issue thread.
