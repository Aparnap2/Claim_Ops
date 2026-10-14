# Production Readiness Test Matrix

## Scope

- **Baseline commit**: `main@0f33724ddc0c4ed0cc04292a93100ec2b495946f` (HEAD, merge of APA-83 P12, CI run #38511713891 success).
- **Validated at**: CI run IDs `gh 38511713804` (`ci`), `38511713891` (`integration`), `38511713908` (`security`), all green at the above SHA on 2026-10-14.
- **Out of baseline**: PR #159 at `879771b` (APA-90, open, green checks on the PR head). Its changes are verification-on-PR-head only and are **excluded** from the baseline coverage numbers below. Re-anchor when merged.

## Non-negotiable evidence rules

- Skipped ≠ covered. `0 passed / 20 skipped` is recorded as 0 passed.
- A declared transition in `claims/statemachine.go` is not a production path until a production caller exists; rows carry an explicit **evidence status**.
- This document is a read-only audit at baseline; no production change is made here.
- Relation references: Linear issue IDs are canonical (`APA-86`, `APA-87`, `APA-88`, `APA-89`, `APA-91`).

## Result vocabulary

- **passed** — executed, assertion green.
- **failed** — executed, assertion red (kept as evidence, not edited away).
- **skipped** — executed but skipped due to absent PG/Mockoon/localgcp; does not count as coverage.
- **structurally absent** — no production path exists to exercise at all; separate finding by Linear ID.

---

## Seeded baseline gaps

### A. `internal/repository/postgres` — 0 passed / 20 skipped without live PostgreSQL

- Existing coverage: 20 tests in `internal/repository/postgres`.
- Execution at baseline: `0 passed / 20 skipped / 0 failed` locally when `TEST_POSTGRES_DSN` unset; `20/20 passed` under `TEST_POSTGRES_DSN`.
- Exact CI evidence: `d1d4937` — integration run #38246270052 passed with 10/10 gated packages; added fail-closed gate `scripts/ci/check_pg_gated_packages.py`.
- CI/staging: fail-closed gate now aborts if a gated package only skips. Per-package assertion must be independently verified for any *new* test added to one of the 10 packages — do not assume it propagates.
- Missing scenario: gate assertion + every-puts-pass equality check; smallest slice under APA-83 P10.
- Expected durable state: no new durable state.
- Release impact: hygiene / precondition, not blocking by itself; gate-trust before APA-88.
- Evidence status: **verified under live PG; skipped locally**.

### B. `RoutingHITL` — zero production consumers

- Existing coverage: `internal/sufficiency/sufficiency.go` (routing decision, unit `sufficiency_test.go:108,147,260,282`); `internal/worker/processor_sufficiency_bridge_test.go:28` (envelope assertion).
- Result: passed *as a unit/routing decision*. No `Routing` field is read outside the sufficiency package (grep at baseline).
- Missing scenario: a claim reaching a human-actionable state after insufficiency. Routing signal currently routes **nowhere** in production.
- Expected durable state: claim row in human-actionable HITL state, persisted + tenant-scoped (absent).
- CI/staging: requires live PG (claim persistence) but no localgcp.
- Evidence status: **structurally absent (product path)**. See APA-91; corrected APA-88 scope in its comment.

### C. `internal/app` — localgcp/GCW emulator gap is an *environment-qualification* gap

- Existing coverage: 3 emulator-backed tests still skip with Mockoon + PostgreSQL healthy.
- Result: skipped (presence of localgcp + GCS) — never counted as pass.
- Exact CI evidence: P12 integration run #38511713891 (success); `internal/app` excluded from Mockoon required set in `scripts/ci/check_mockoon_gated_tests.py`.
- CI/staging: cannot pass without `infra/localgcp/`.
- Expected durable state: no new state; precondition for integration campaigns, not in-frame.
- Release impact: blocks environment-qualification, tracked as KL-01/KL-02/KL-03.
- Evidence status: **environment gap, not test correction** (per P12 decision).

### D. No production path from insufficient evidence to human decision (APA-91 — High, new)

- Evidence status: **structurally absent**; missing capability, not a test gap.
- Call-graph at baseline (re-verified): `workflow.Apply` (0 production callers; only a doc comment in `internal/invest/doc.go:39`); `claims.NewClaim`/`postgres.SaveClaim` (0 production callers); `ClaimStatusActioned` (never set in prod, is gated input); `cmd/api/main.go` registers exactly one route (`POST /v1/claims/:id/decision`); sufficiency envelope is built but never crossed into a claim state a human can act on (details in APA-91).
- CI/staging: downstream row, when landed, will need live PG + tenant context but not localgcp.
- Expected durable state: yet to be designed (AC#2 of APA-91).
- Release impact: **blocks qualification**; now a stated reason in the release gate.
- Link: APA-88 corrected via comment, ref APA-91 as dependency; do **not** write the connected test against manufactured state.

---

## Matrix rows (all nine fields per user schema)

Legend: *E.S.* = evidence status (verified-at-baseline / PR-head / documented-gap / unverified).

### Row 1 — Pull claim transitions through `/v1/claims/:id/decision`

- Invariant: a state transition must be admitted only by `claims.CanTransition`, version-CAS checked on the row.
- Existing coverage: `internal/handlers/decision_integration_test.go` (smallest reported shrinkage only; PG-gated); `internal/claims` 15 unit tests + `internal/workflow` 51 unit tests. Runner: `go test ./... / PostgreSQL-gated`.
- Execution: decision test works only under `TEST_POSTGRES_DSN`; otherwise **skipped**; structural unit set **passed**.
- Missing scenario: an integration test that **starts** from insufficiency, not one that injects a pre-built `HITL` claim; currently zero.
- Expected durable state: new claim version + `claim.transitioned` event, CAS row updated; HMAC + idempotency event.
- CI/staging: required live PG + (when wired) CORS/HMAC + tenant; requires APA-91 path.
- Evidence: `decision_integration_test.go:38-53` skips without PG; README/ADR for form and tenant (see code referenced); run #38511713891 on `main`; integrated.
- Release impact: human authority (blocking).
- Evidence status: **documented-gap** at baseline (connected row missing — see APA-91).

### Row 2 — Sufficiency routing discriminates sufficient vs insufficient

- Invariant: only insufficient evidence carries the HITL routing; sufficient evidence must not.
- Existing coverage: `internal/sufficiency/sufficiency_test.go:260,282`; `internal/worker/processor_sufficiency_bridge_test.go:28`; golden behavior tests in `tests/unit/test_golden_deterministic.py`.
- Execution: passed (unit set in `go test`).
- Missing scenario: end-to-end claim transition; plus golden regression check on the Python side (PR #159 sharpens it — not baseline).
- Expected durable state: insufficiency becomes an `R8` exception (processor path); sufficient set remains without routing.
- CI/staging: **mocked HTTP** (CI), no live provider.
- Evidence: sufficiency unit + bridge assertions; baseline coverage count at `go test ./...`. Evidence status: **verified at baseline** for routing decision; **documented-gap at the product path** (APA-91).

### Row 3 — PostgreSQL / RLS tenant isolation

- Invariant: any read or write at the DB boundary must be scoped to `claims.TenantID`; cross-tenant reads must fail.
- Existing coverage: prefixed by `internal/auth/tenant` + `internal/repository/postgres/decision_integration_test.go` (tenant handling in `postgres/{BeginTenantTx,WithTenant}`).
- Execution: unit surface passes; integration surface **skipped** without PG.
- Missing scenario: soak tests for cross-tenant access / loud failure on a missing tenant scope; none currently asserted as a failure visible to CI.
- Expected durable state: unchanged claim casing; row enforcement via `WHERE tenant_id` filter.
- CI/staging: requires live PG; no localgcp.
- Evidence status: **documented-gap** (assertions present, cross-tenant dedicated coverage not present).

### Row 4 — Idempotency and concurrent decision submission

- Invariant: a decision is idempotent under the idempotency key; a concurrent / out-of-order submission receives a clear failure, never silent double-apply.
- Existing coverage: decision handler idempotency paths in `decision.go` + `decision_integration_test.go` (version & idempotency); workflow idempotent replay (unit).
- Execution: skipped without PG (live).
- Missing scenario: stress (N concurrent submissions) to prove no double apply; no soak test today.
- Expected durable state: exactly one state mutation/event appended, replay returns identical claim, no duplicate.
- CI/staging: PG only.
- Evidence: referenced but not enumerated in this commit; gap outside matrix subset.
- Evidence status: **documented-gap**.

### Row 5 — Uncertain remote outcomes in workflow recovery

- Invariant: a 409 `CreateOrUpdateExecution` must find/create (GCW specifics); crash-window resolution must not duplicate.
- Existing coverage: `internal/workflow/gcw_provider.go:117-129,182-183`; `provider_test.go:207-242`; `apa41_resource_name_test.go:184-201` (cert stub) only.
- Execution: GCW emulator cannot return 409 on v0.5.0 — tracked KL-02 / APA-40.
- Missing scenario: managed-service equivalence test.
- Expected durable state: no duplication, resume of document provisioning only.
- CI/staging: cloud workflow credential absent / emulator blocked (manual validation only).
- Evidence status: **blocked** (KL-02).

### Row 6 — Claim workflow envelope and transition ownership

- Existing coverage: `internal/workflow` (51 unit tests); `claims.CanTransition`; generic driver `workflow.Apply`.
- Execution: unit surface **passed** at baseline.
- Missing scenario: enumerate which production caller initiates each transition. Per APA-91 baseline, production callers are currently absent for the human-authority chain.
- Expected durable state: versioned claim update plus one `claim.transitioned` event per transition once `APA-91` lands.
- CI/staging: PostgreSQL plus tenant context; no localgcp dependency for the new producer once designed.
- Evidence status: **documented-gap** (state machine exercised; production ownership not yet traced).

### Row 7 — PII leakage + secret redaction

- Invariant: raw `pii` / un-redacted fields never leave the system plane.
- Existing coverage: `internal/observability` package; APA-89 added `pii_test.go` plus 4 new mypy cases (merged); APA-86 open.
- Execution: passed (unit) when covered; APA-86 stays to report; matrix treats it as gap.
- Missing scenario: structural lint driven by golden + static string search for literal leaked forms; coverage absent.
- Expected durable state: no persistence of unmasked fields.
- CI/staging: unit tests plus the APA-83 P5 secret guard under ordinary CI; no external services.
- Evidence status: **documented-gap** (APA-86). (PR #156/P8 added race on concurrency.)

### Row 8 — Audit integrity / decision actor attribution

- Invariant: every decision carries an auditable actor Id and signet-pinnned HMAC.
- Existing coverage: `decision.go` uses signed Webhook/HMAC requests and a RLS transaction; persisted audit relies on the event-store path still to be inventoried.
- Execution: unit/contract surfaces passed at baseline; the PG-backed `decision_integration_test.go` is skipped without `TEST_POSTGRES_DSN`.
- Missing scenario: persistent actor audit query with user / timestamp / source; no test shown.
- Expected durable state: no mutation without attribution.
- CI/staging: requires PG + DSN; no live provider.
- Release impact: auth/audit, blocks qualification.
- Evidence status: **documented-gap** (conn. auth/ audit policy not structurally tested).

### Row 9 — Deployment compatibility, rollback, operational recovery

- Invariant: an image rollback leaves no uncommitted or partially committed business state, and post-deploy smoke confirms server health.
- Existing coverage: CI `.github/workflows/deploy.yml` template shows post-deploy blog smoke but has not been proven to match the app.
- Execution: **no running-proof** — deployment remains deferred (P2).
- Missing scenario: post-deploy smoke and one controlled rollback rehearsal; external deployment pipeline required.
- Expected durable state: no unexplained state drift; deterministic artifact reference.
- Release impact: external gating, not code-internal alone.
- Evidence status: **not-run** baseline.

### Row 10 — Mockoon gate closure (APA-83 P12)

- Invariant: any Mockoon-declared test must actually execute each mock-dependent case or gate fails.
- Existing coverage: gate checks declared `skipped()` and aborts.
- Execution: passed (0f33724 Actions, #38511713891).
- Missing scenario: extends internal/app once localgcp exists.
- Evidence status: **verified at baseline** (on CI; 82 mock-required cases executed and fail-closed controls verified before merge).

### Row 11 — Live-provider build isolation (qual path)

- Invariant: build-tag isolates the qualified tests; guard proves no accidental provider call from non-qual targets.
- Existing coverage: `scripts/ci/check_qual_live_build_isolation.py`, `tests/unit/test_qual_live_build_isolation.py`; `check_qual_live_provider_calls.py`.
- Execution: suite passed in prior cycle (`9625199`).
- Evidence status: **verified at baseline** (no new live investigation; unchanged scripts/functions).

### Row 12 — Live-provider fail-closed and strict type/hint gate

- Invariant: only deterministic mode for provider; bounded fallback.
- Existing coverage: APA-89 (merged at `e68cf7e`) plus strict mypy under APA-83 P9 (file-level typing closure). Groq behaviour itself lives in the live-qualified suite, not this baseline.
- Evidence: merged `fa8b98f`; refer docs.

---

## Release gate summary

**Status: NOT QUALIFIED.** Documented gaps and blocking items:

1. **Missing capability**: APA-91 (no production path from insufficiency to human decision) — structural rather than evidential.
2. **Missing connected test**: APA-88 (corrected scope, dependency APA-91).
3. **Unresolved rule authority**: APA-87 (validation vs verify, transitions/NCGuard offset).
4. **PII/untested**: APA-86 + a missing redaction structural sweep.
5. **Blocked**: APA-37, APA-40, APA-48 (GCW retry/409 semantics; worker image rollout).
6. **Live low-capacity / definition**: KL-04/KL-09 429s, APA-55 1/5 run; APA-56 causal half (MissingEvidence not observed).
7. **Deployment path**: deferred P2, matrix row 9 not yet exercised (no real deployment).

This matrix is the baseline only; PR #159 must be folded into evidence once it merges.
