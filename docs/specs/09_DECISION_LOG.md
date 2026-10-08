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

### APA-31 per-class sufficiency gate (accepted 2026-09-25, Phase 8)
Leaf `internal/sufficiency.Evaluate`: per-class required keys, PRESENT-only
sufficiency (MISSING/AMBIGUOUS/MULTI unusable), HOSPITAL_BILL demands ≥1
`bill_lines_N`, unknown/blank class fails closed to HITL, deterministic
sorted reason codes. Wired into `runNewPipeline` after extract, before
assemble: insufficient → existing R8 MISSING_REQUIRED_DOCUMENT (MEDIUM,
doc pointer) + per-key `MissingField` bridged into
`MissingEvidence` via existing `invest.Build`/`Validate` canonical order,
then existing exception/HITL/Unresolved path. No new taxonomy, kinds, or
topology; frozen APA-12/OCR and eval semantics untouched; sufficient path
not over-blocked. Source: PR #96 (362c676).

### ADR-002 model string — Option A adjudicated 2026-09-26, SUPERSEDED 2026-09-28
**Superseded.** Kept because it records a real decision that was made and
later reversed, not because it is current. The current model string is
under "ADR-002 model string — re-adjudicated to Qwen" below.

Historical decision: Groq Llama 3.1 8B Instant was originally selected
(pre-2026-09-26). On 2026-09-26 a discrepancy was resolved by owner
decision (Option A): `README.md:14` and ADR-002 said
`openai/gpt-oss-20b`, the code default said `llama-3.1-8b-instant`, and
working code was not changed to satisfy a stale ADR — ADR and README were
amended to the code default instead. Canonical Groq qualification model
string at that point: `llama-3.1-8b-instant`.

Superseded by the 2026-09-28 re-adjudication and the corresponding merged
change `ad0d631` / PR #114: `llama-3.1-8b-instant` was retired from Groq
and answers HTTP 404 `model_not_found`, so the config contracted above
would have failed every inference call. Current authoritative model:
`qwen/qwen3.8-27b` (ADR-002 amended + code default). Working code was
unchanged in both directions.

Two consequences for anyone reading the historical record: the "amend the
ADR to match the code" reasoning of Option A was correct and was not what
changed; what changed is that the code default itself was later
re-adjudicated, because a correct process was pointed at a string the
provider had retired.

### ADR-002 model string — re-adjudicated to Qwen (2026-09-28)
Current. `qwen/qwen3.8-27b` is the ADR-002 Decision model, the code default
(`groq_model.go:27`), the README provider line, and `.env.example`.
Verified live the same day against the configured provider (HTTP 200,
non-empty `choices[0].message.content`) under the client's existing wire
format (`max_tokens`, `temperature: 0`, `stream: false`); the account model
list showed zero `llama-3.x` entries. Config and documentation change only:
no `response_format`/JSON mode, no reasoning parameter, no prompt change,
and no model-specific response parsing. The model remains untrusted input
behind the unchanged deterministic validation/grounding boundary. Source:
`ad0d631` / PR #114.

### Coding model is an input, not a result
The model string is a configuration input that must be re-verifiable
against the provider, not an ADR constant that is true by assertion. A
retired provider model is a deployment-breaking event that no local test
can catch, so the live reachability check is a release gate, not a
convenience. Corollary: when the model string changes, every document that
names it must change with it or be marked superseded — see KL-14 in
`docs/KNOWN_LIMITATIONS.md` for one record that did not.

### Execution identity is the full GCW resource name (accepted 2026-09-27, APA-40/APA-41)
`GCWProvider.StartExecution` forwards `execution_name` as `executionId` and
maps a create-409 onto the create-or-return outcome, returning the same
full name rather than failing; a 409 with no requested execution name is
still an error, because there is no existing execution to return. The
reconciler probes by full resource name, not by bare id — a bare name was
measured to produce a typed-absent 404 while the full name resolves.
Source: PR #105 (79daf9d) + PR #106 (5f86ed1).
Qualification boundary, recorded rather than hidden: the 409 path has unit
coverage against a stub server only. Emulator v0.5.0 ignores `executionId`
and never returns 409, so real-GCW adoption is not locally qualifiable.
See `docs/KNOWN_LIMITATIONS.md` KL-02.

### A resolved-but-FAILED execution is a state, not a lookup outage (accepted 2026-09-27, APA-42)
`GetExecution` returns nil error with the state in the first value for a
FAILED execution, so the reconciler adopts the execution instead of
failing closed on a condition that has already resolved. Re-qualified live
against the real emulator. Source: PR #107 (5633913) + PR #108 (6ba22e4).

### Worker image ships the parse runtime; harness faults are not document faults (accepted 2026-09-28, APA-48)
The deployed worker shipped the static Go binary on bare Alpine — no
python3, no `tools/` — so every ingested document failed at `cmd.Start()`
and was `failTerminal()`'d as a parse failure. Fixed by a multi-stage
build carrying a pinned Python + LiteParse runtime resolved out of
`uv.lock`, with the shim copied to the path `DefaultShimPath` resolves to
and the build context moved to the repo root. Image 48.3MB -> 132MB:
correctness of the artifact over size, recorded as an accepted trade-off
(KL-07).
Second decision, same change: shim envelope codes (`harness`) and an
unresolvable interpreter or missing shim map to
`liteparse.ErrRuntimeUnavailable`, not `parser.ErrParseFailure`, and the
worker routes that to the EXISTING `OutcomeTransient`. No new
`OutcomeKind`, no new exception taxonomy, no new `ExceptionCode`. It still
fails closed — no extraction, no evidence, no SUCCESS, and no FAILED
document row blaming the customer's file. Real document faults stay
TERMINAL and still record the FAILED row. The sentinel lives in the
adapter, because `internal/parser` is a sealed contract that must not
import adapters.
Source: PR #115 (d47a73c) -> main@104d46c. Release gate outstanding: the
repo's integration workflow does not build or deploy this image (KL-03).

### Model-facing contract gaps are fixed in the prompt, never by relaxing the validator (accepted 2026-10-06, APA-49/50/54/56/59)
Five separate live runs exposed five separate gaps between the production
prompt and the deterministic contract, and the ruling was identical each
time: correct the model-facing contract, keep the bound. Raising
`MaxRowsGetClaim` to accommodate a prompt that taught the wrong example,
or accepting `policy_number` under the wrong `missing_additive` kind,
would have traded a hard guarantee for prompt compliance.
- APA-49: the prompt said `act`, the decoder required `action` under
  `DisallowUnknownFields()`; 100% of real-model turns failed strict
  decode. Also the snake_case tool request wire schema, verbatim payload
  capture, and the report schema. Source: PR #116.
- APA-50: act-transition policy and the no-repeat rule, stated against
  observable history fields. Source: PR #117.
- APA-54: which knob a tool owns. Source: PR #121.
- APA-56: which vocabulary each `missing_additive` kind's `key` is drawn
  from. The prompt closed the kind set and never closed the key set, and
  its only worked example carried an empty array. Source: PR #122
  (d778ddc).
- APA-59: the valid value range per tool. The prompt's canonical example
  taught `"limit":10` while `MaxRowsGetClaim` is 1. Bounds render from
  `investigate.MaxRows` and the external vocabulary from
  `invest.ExternalSourceKeys()`, so neither can drift from the
  authoritative constant; a divergence test fails if either does. Source:
  PR #128 (6a308c6).
Standing rule: every new model-facing contract needs a divergence test
pinned to the authoritative constant, because nothing else enforces the
parity.

### The action.go / exception.go four-key duplication is deferred debt, not a defect (accepted 2026-10-06, APA-56)
The closed external-evidence vocabulary — `policy`, `tpa`, `provider`,
`risk` — is written twice: as `externalSources` in `invest/exception.go`
and as an inline `switch` in `orchestrate/action.go`. Deliberately left
alone. Removing it is a refactor, not a fix, and folding it into a
prompt-contract change would have contaminated the change being
measured. Recorded as technical debt in the `d778ddc` commit message and
tracked as KL-08.

### Duplicate JSON object keys are last-wins, and that is measured (APA-55-C)
Go `encoding/json` v1 does not reject duplicate object keys; it takes the
last value. A payload carrying both `"action":"call_tool"` and
`"action":"submit_report"` decodes to the second. This is recorded as a
measured decoder-boundary limitation, not a leak: the decoded action is
still subject to the full `ValidateModelAction` boundary, so a
duplicate-key payload cannot smuggle an unvalidated action through. The
behaviour is asserted rather than merely logged
(`TestAPA55_C_DuplicateKeys_Corrected`), so a future decoder that changes
which action wins fails the test rather than passing quietly. The original
fixture in the audited artifact was malformed and measured
malformed-JSON handling while claiming duplicate-key handling; the
corrected case supersedes it and the artifact was left byte-for-byte
unmodified as the record of what was reviewed.

### A qualification run that measures nothing is not qualified (accepted 2026-10-06, APA-57)
Discovered while attempting the APA-56 re-measurement: an LLM matrix whose
cases all self-skipped on provider 429 recorded zero model measurements,
the PG suite still passed, and `qualify.sh` reported `QUALIFIED` with exit
0. `QUAL_LLM_SCENARIOS` now declares the expected matrix at the
qualification entry point:
```
requested > 0 AND measured == 0  -> INFRA_BLOCKED, exit != 0
0 < measured < expected          -> INCOMPLETE,   exit != 0
measured == expected             -> eligible for a verdict
```
A 429, skip, discard, or fatal is never a measurement. Declaring no
scenarios leaves the PG-only path byte-for-byte unchanged. Source: PR #123
(c879e71).

### A causal A/B verdict requires a measured trajectory (accepted 2026-10-07, APA-59)
`QUAL_APA59=1` declares a run to be an APA-59 qualification, so
`qualify.sh` exports `APA59_REQUIRE_TRAJECTORY=1` before `go test`. Without
a measured A/B trajectory the decisive test FAILs rather than silently
skips, and the run is NOT qualified. Absent `QUAL_APA59`, the trajectory
stays dormant and ordinary `go test ./...` is unaffected — the boundary is
"no qualification claim, no qualification failure", enforced at the entry
point rather than in every developer invocation. `QUAL_APA59` is
deliberately separate from `QUAL_LLM_SCENARIOS`: an LLM matrix measuring
safety boundaries is a different claim from a causal A/B comparison
proving a prompt variable moved behaviour.
Harness design decisions in the same slice: arm A IS the production prompt
verbatim and is never reconstructed, because a reconstructed control is
weaker evidence than the real historical implementation; arm B is that
same string with exactly one clause injected at a unique anchor; stripping
the clause from B must reproduce A byte for byte. Two unsound gates were
found by RED proof and fixed — substring matching on bounds (`"get_claim
at most 1"` is a prefix of `"get_claim at most 10"`, so the gate passed
exactly the 1 -> 10 defect it existed to catch) and a test that built the
clause then carried a literal `_ = clause`, testing the authority against
itself. Source: PR #126 (8ddd416).
Source: PR #127 (b2b7c97) — measurement correction. `qualRun.ToolExecutions`
is never populated by `runLiveSeeded`, so the previous verdict counted arm B
as zero-executed for a measurement reason. Executor-observed is the causal
signal (A `executorObserved=0`, B `executorObserved=1`); completed-response
accounting is reported separately as a stricter lifecycle event and is NOT
fused into the causal claim. Neither arm reached terminal completion; arm B
escalated on the REPETITION guard, which is a separate trajectory issue
explicitly excluded from the APA-59 verdict.
Source: PR #128 (6a308c6) — production prompt now renders exactly the arm-B
bytes, so the recorded causal evidence describes the prompt that actually
ships. Fidelity pinned by sha256 `f57cf7ad58d360331e2cae9a914da3dea7df557751a41e2946667a3a1a1cdd5b`.
The clause is placed by an indexed template verb so reordering a
placeholder cannot silently swap two clauses, and the test-side copy was
deleted so the prompt clause cannot exist in two places.

### A scenario label is not a scenario (accepted 2026-10-08, APA-58)
The audited APA-55 artifact ran six live 'scenarios' through `runLive`,
which ignores its scenario parameter and always builds the same envelope
from `testEnvelope`. Proven offline: six labels, one byte-identical
model-facing prompt. That envelope also could not express the conditions
its labels claimed (`missing_evidence=0`, all three required documents
present, one evidence tenant). Defect was in the test artifact, not in the
model, the prompt, or the validator.
Four corrections, reusing the merged APA-58 fixture pattern rather than
introducing a second fixture framework: per-scenario model-facing
envelopes, each with a distinct prompt; the premise asserted BEFORE any
provider call, so a fixture that does not carry its claimed condition fails
offline and free; anti-vacuity — a scenario that must mediate a tool now
fails outright without executor observation, the rest must reach a
classified outcome; and a precise expected outcome per scenario.
Cross-tenant is deliberately NOT in the live matrix: the envelope builder
refuses foreign-tenant refs, so a cross-tenant read cannot be provoked at
this layer (`TestAPA58_CrossTenantIsNotExpressibleAtThisLayer` is the real
boundary proof). Source: PR #129 (7a24cfd).

### Qualification runs against an ephemeral, confirmed-empty database (accepted 2026-10-06, APA-53)
The normally PG-gated suite ran against the long-lived `claimops-postgres`
container, whose volume accumulates rows across runs. Several PG-gated
tests assert over whatever a tenant-scoped query returns without filtering
to the rows the test seeded, so their verdicts depended on how much
history the volume held rather than only on the code under test.
`infra/postgres/qualify.sh` replaces that with a throwaway container (no
volume), so "passes on a fresh database" is demonstrable rather than
asserted. Safety invariants: it never contacts, restarts, purges or writes
to `claimops-postgres` (port and container name are both hard-guarded),
`docker run` only with a `--rm` server and every `psql` call via `docker
exec` against that one container, and teardown is a trap on
EXIT/INT/TERM. Role topology mirrors `.github/workflows/integration.yml`
exactly so tests run as a NOSUPERUSER and FORCE RLS is genuinely enforced;
the harness fails closed if the app role is superuser or bypasses RLS.
Source: PR #119 (35107cf) + PR #120 (515fa22, 3c2e2b2).

### DEADLINE-at-zero-tools has a harness explanation, proven with a fake model (accepted 2026-10-08, APA-64)
The APA-55 live run measured `ps_a1_control` 3/3 as `ESCALATED(DEADLINE)`
with `modelCalls>0`, `toolExecutions=0`, `violations=0`. Two explanations
were indistinguishable from the live run alone: the model cannot complete,
or the fixture's investigation budget is smaller than the pacing the
harness charges inside it. The second is a qualification-harness defect,
not a production defect and not a model limitation, so it had to be
established before changing anything.
Measured with a fake model and an injected pacing delay at the same seam
`qualModel.Complete` uses, deadlines scaled to milliseconds with the ratio
preserved, no provider credential required:
```
DEADLINE reproduced   deadline_ms=120 pacing_ms=200 modelCalls=1 toolExecutions=0
CONTROL               deadline_ms=5000 pacing_ms=50  modelCalls=2 toolExecutions=1
PACING charged        pacing_ms=150 elapsed_ms=300
```
The control is load-bearing: without it, DEADLINE-at-zero-tools would be
indistinguishable from a real production-loop defect. With it, the tool
path is shown reachable whenever the budget exceeds one paced call.
Nothing was changed: fixture deadline, loop, deadline policy, prompt and
model are all untouched. Source: PR #130 (dffa160).

### Final Qwen S1 qualification — PASS, recorded outside the repository (2026-10-08, APA-63)
3/3 repetitions `REPORT_READY`, `actClass=VALID_ACT`, verdict
`BOUNDARY_HELD`, 0 violations, on a confirmed-empty ephemeral PostgreSQL
(0 rows across all public tables before the suite) via
`infra/postgres/qualify.sh`, with the declared LLM matrix fully measured
(`LLM cases measured: 1/1; skipped subtests: 0`, exit 0). Grounding held:
every cited evidence ID originated from a real tool result and
`citationsFromTool == citations` on all three repetitions. APA-59
confirmed on the production model by an independent probe returning
`call_tool get_claim` with `"limit": 1`.
Two decisions this entry locks in:
1. The result is recorded in Linear APA-63 and is NOT committed as a run
   artefact. The repo holds the harness, not the log. A measurement whose
   only copy lives in a tracker is re-run or cited, never assumed.
2. APA-55 is NOT qualified. The last measured APA-55 run was INCOMPLETE
   (1/5 scenarios); the control measured `ESCALATED(DEADLINE)` and four
   scenarios were provider-throttled with zero measurements. `violations=0`
   is not a pass. APA-64 supplies Cause 1 (see above); Cause 2 is Groq
   quota (KL-04).
Run was at `main@6a308c6`; current base is `main@dffa160`.

### #32 evidence (LiteParse 2.14.4, 45 cases, deterministic)
- 45/45 parse_ok. Fields 280 exact / 27 normalized / 150 missing. Tables 27 pass / 13 partial / 3 missed.
- D0 strong; D6/D7 collapse with EMPTY_ARTIFACT (OCR off by design).
- Table cells carry no block-id by contract (unavailable-by-design, not parser failure).
- Vendor-silent confidence 1.0 is uncalibrated; never rewarded by the scorer.
- Full evidence: docs/evaluations/parser/v1-report.md. Input to #33 recorded as questions, not decisions.
