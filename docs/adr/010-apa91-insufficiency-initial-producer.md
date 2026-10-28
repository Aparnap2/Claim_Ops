# ADR 010 — Insufficient-Evidence Production Owner and Initial Durable State

* Status: Accepted (limited scope) — distinct decision for initial insufficiency only
* Base: `main@0f33724ddc0c4ed0cc04292a93100ec2b495946f` (recorded baseline; current HEAD reverified at creation)
* Related: APA-91 (High, open), APA-88 (corrected scope), existing lifecycle in `internal/claims/statemachine.go`, `workflows/claim-investigation.yaml`
* Related code paths: `internal/worker/processor.go::runNewPipeline` sufficiency path; `internal/handlers/decision.go::DecisionHandler`; `internal/repository/postgres/{claim_repository,hitl,transaction}.go`; `internal/workflow/workflow.go::Apply` (test-only usage)

## Context

APA-91 records a capability gap, not a test gap:

- `sufficiency.Routing` (`RoutingHITL`) is defined in `apps/api/internal/sufficiency/sufficiency.go` (lines 64–67, 204, 241, 243) but is never read in production code.
- `runNewPipeline` invokes `sufficiency.Evaluate`, synthesizes an R8 finding, builds an exception envelope, and calls `Launcher.EnsureLaunched`, but never durably transitions the persisted claim row.
- `workflow.Apply` has zero production callers (`workflows/claim-investigation.yaml` is the orchestrator's external HTTP path, not a Go mutation).
- `claims.NewClaim` / `postgres.SaveClaim` have zero production callers.
- `claims.ClaimStatusHITL` is a decision *input* in `handlers/decision.go:94`, not a producer.
- The external decision endpoint (`cmd/api/main.go:60–75`) is the only runtime entry into state transitions, and its mutation matrix is action-based.

The only deterministic, tenant-scoped, version-checked mutation templates that exist in production are:

1. `handlers.DecisionHandler`: `BeginTenantTx` → `LoadClaim` → `claims.Transition` → atomic `UPDATE claims SET status, version WHERE ... AND version = $old` → `AppendEvent` (idempotent on tenant/claim/event) + `audit_log` insert → `Commit`, or rollback and error on failure;
2. `repository/postgres.ClaimRepository.SaveClaim` with optimistic `WHERE claims.version = $oldVersion` (used elsewhere via ClaimBridge, but the decision handler bypasses it with a raw CAS update).

No Go production code writes the lifecycle edges `EXCEPTION → INVESTIGATION_REQUIRED`, `INVESTIGATION_REQUIRED → WAITING_FOR_EVIDENCE`, `WAITING_FOR_EVIDENCE → HITL`, except the `claims` state machine declarations/hitl.go query for listing pending rows. Reachability evidence:
- `DecisionHandler` action map: `APPROVE→VERIFIED`, `REJECT→CLOSED`, `ESCALATE→EXCEPTION`, `HITL/HOLD→HITL`, `EXPIRE→EXPIRED`.
- legal edge `ESCALATE→EXCEPTION` is allowed only from `VALIDATING` (per `Transitions`), so a claim already in `EXCEPTION` cannot be escalated back to exception.
- No `APPROVE/HITL/REJECT` allowed from `EXCEPTION`; no action creates `INVESTIGATION_REQUIRED` or `WAITING_FOR_EVIDENCE`. Thus the documented workflow cannot drive those intermediate transitions through the public decision API today.

There are therefore two distinct questions: (1) the initial insufficiency producer (who writes the first durable exception), and (2) the owner of the subsequent exception-to-review edges. Question (1) is concretely answerable from repository evidence. Question (2) is not.

## The decision required: Option A vs Option B

### Option A — Existing lifecycle (accepted for the initial transition only)
- Initial durable producer: Go deterministic `worker.Processor.runNewPipeline` (`apps/api/internal/worker/processor.go:886–995`).
- Initial durable target state: `claims.ClaimStatusException` (existing, already declared in `claims/statemachine.go`).
- The state edge used is the existing, intentional production edge `claims.ClaimStatusValidating → claims.ClaimStatusException`, validated by `claims.CanTransition` and `claims.Transition`.
- Intermediate progression `EXCEPTION → INVESTIGATION_REQUIRED → WAITING_FOR_EVIDENCE → HITL → ACTION_PENDING` remains on the existing state vocabulary; no state is invented. The workflow owns subsequent steps via its investigation + human-callback phases, but this ADR records that the **present** Go boundary does not expose a documented mutation command for those intermediate edges.
- The human-review handoff is delivered through the documented Cloud Workflow (`await_human`/`apply` callback into `/v1/claims/:id/decision` with `APPROVE/REJECT/ESCALATE/HITL/HOLD/EXPIRE`), with timeout producing `EXPIRE→EXPIRED` as a terminal, audit-attributable outcome (APA-26).

### Option B — Explicit parking state (rejected at this time)
- Would introduce a new status (e.g. "parked awaiting review") not present in the `claims` vocabulary.
- The existing `EXCEPTION` state already denotes "escalated because evidence is insufficient" according to the workflow taxonomy (R8) and the sufficiency gate. Creating an additional parking state would shadow `EXCEPTION` rather than represent a new product distinction.
- Repository evidence does **not** justify an additional state: no query, approval matrix, or invariant requires a state distinct from `EXCEPTION` for the first transition. `hitl.go` queries both `HITL` and `ACTION_PENDING` for “pending HITL”, not a parking state.
- Accepting Option B would require an ADR that also updates the state machine, the KV/envelope contract, API actions, and GCW workflow in lockstep. None of that is currently scoped or evidenced.

## Decision

**Option A is accepted for the initial insufficiency transition only.**

Durable target state for an insufficient-evidence outcome: `claims.ClaimStatusException`.

Authoritative producer: `worker.Processor.runNewPipeline` (Go deterministic service), writing the claim row inside a single tenant-scoped transaction on the insufficient-evidence branch before the GCW launch can succeed without a durable claim-state mismatch.

The transition is dispatched from the worker, not from the agent cognitive loop. The agent remains read-only (ADR 008; agent-mutation boundary). Event IDs and tenant are derived deterministically and not model-derived.

## Transaction boundary (binding for the implementation phase)

On the insufficient-evidence branch, inside one `BeginTenantTx(ctx, tenant)`:

1. `LoadClaim` (rebuild `ProcessedEvents` from `claim_events`, as `decision.go` already requires).
2. `claims.Transition(claim, claims.ClaimStatusException, deterministicEventID, expectedVersion, tenant)`.
3. If `TransitionError.Code == CodeIdempotentReplay`, commit with no mutation and report an idempotent Outcome (no duplicate audit/event).
4. Otherwise persist the new claim via `SaveClaim` CAS update (`WHERE claims.version = $oldVersion`; `ErrVersionConflict` on loss) using the same tenant-scoped transaction.
5. Append an event atomically (`AppendEvent`, idempotent on tenant/claim/event).
6. Atomically append an audit row consistent with `audit_log` writer expectations (actor = `worker`, action = `state_transition`, entity `claim`, before/after version).
7. Commit.

**Launch discipline (`ensureLaunched` remains outside the claim Update transaction, but after commit):** envelope + GCW launch are best-effort per S6. If the persisted transition succeeds but launch is lost, redelivery re-enters `EnsureLaunched` (idempotent by investigation ID) — it never rolls the claim back. If any of steps 1–7 fails, the worker must **not** call `EnsureLaunched`, and the Outcome is transient (rollback; no partial durable state). Sufficient evidence must remain completely unchanged (no exception row, no audit, no workflow launch).
The only persistent row that may exist before the transaction is the envelope persisted by the GCW launcher in S6, and only *after* the S6 launch path is reached; the initial producer check must never launch before the claim transition transaction. Indeed the implementation order must be: claim transaction first; launch only on committed outcome.

Required ordered steps:

1. Build `suffFinding` first, build assembly/verification baseline *but only tenant-scoped claim read* is needed to version.
2. Execute claim-state transaction (`BeginTenantTx / LoadClaim / claims.Transition / SaveClaim(CAS) / AppendEvent / audit insert`) with rollback on any error.
3. Only on successful or idempotent-replay outcome, proceed with `buildExceptionEnvelope` and `Launcher.EnsureLaunched`.
4. On transaction failure, Outcome surfaces a terminal/transient store error, and `ExceptionCodes` still carries R8 only if a fallback is appropriate; but the claim row stays unchanged, and no launch occurs.

### Outcome shape for the journal’s perspective

The processor’s Outcome must record:

- `Outcome.DocumentID`, `Status` (probably `documents.StProcessed`) unchanged.
- ExceptionCodes still include `R8` because evidence is insufficient.
- Its `Attributes`/`ExceptionEnvelope` (if present) correspond to the same `claim_id`, `tenant`, and claim version.
- No `SUCCESS` on Missing/stale/contradictory evidence.

### Idempotency

- Event ID MUST be deterministic, tenant-scoped, and derived only from the claim ID + doc ID + sufficiency bundle hash or reason codes (NOT model output, NOT an external random ID). Same `(tenant, claim, event_id)` replay must cause no new mutation and no new audit/event row.
- `claims.Transition` returns idempotent replay → Outcome STILL logically reports `Status=StProcessed`, `Kind=SUCCESS` (same as today), but the PG state/audit remains untouched.
- A redelivery under the same tenant must produce no new rows; a redelivery under a *different* tenant must fail tenant authorization before any LoadAttempt (must not touch state).
- Replay after a claim was transitioned by a human decision in the same pending row (e.g. EXPIRED) must return a non-usable/err outcome, never re-apply.

### Tenant isolation

- Every repository call in the transaction uses the tenant claims TenantID; `BeginTenantTx` sets `app.tenant_id = $tenant, is_local=true`.
- Cross-tenant submission returns `403 TENANT_MISMATCH` or not-found from the perspective of an API path; in worker terms, the tenant mismatch must produce a terminal failure and no state change.
- RLS must be exercised against the non-superuser runtime DB role per `infra/postgres/qualify.sh` NOSUPERUSER assertions.

### Failure recovery

- Transaction rollback on any repository/store failure; `LoadClaim`/`claims.Transition`/`SaveClaim`/`AppendEvent`/audit all fail-closed.
- No partial writes (no version bump without audit; no audit without version).
- Close-world error reporting: claims error codes `ErrVersionConflict`, treasury Tenant error and map- to terminal outcomes resurfaces all error codes and does not swallow.
- The worker can be redelivered and retries; canonical producer must tolerate undeduplicated retry-then-commit via deterministic event ID + CAS.
- Orphan document rows/markers must not be created on insufficient evidence.

### Human-review handoff

**Unresolved (explicit record, not hidden):**

This ADR establishes the initial producer (`VALIDATING → EXCEPTION`) only. The following subsequent handoff paths are **not** evidenced in the repository today:

1. `EXCEPTION → INVESTIGATION_REQUIRED`: no produced command in Go; the GCW workflow does not carry an action that maps to `INVESTIGATION_REQUIRED`.
2. `INVESTIGATION_REQUIRED → WAITING_FOR_EVIDENCE`: same.
3. `WAITING_FOR_EVIDENCE → HITL`: same.
4. `HITL → ACTION_PENDING` is entered only by an operator calling `/decision` with `HITL` / `HOLD`, *unless* the investigate callback/apply step is modified to derive claim version and post the expected automated transition; today only human intent can progress the state.
5. `ACTION_PENDING → ACTIONED` is not reachable at all from the public decision API (no action).
6. The GCW workflow’s `apply` step forwards a `decision.http_request.body` to the decision endpoint, which can only carry `APPROVE/REJECT/ESCALATE/HITL/HOLD/EXPIRE`. Any mid-chain progression must be represented by one of these actions, which currently only legal targets are `EXCEPTION` (from `VALIDATING`), `CLOSED` (from `VERIFIED`), `HITL` (from `WAITING_FOR_EVIDENCE`), `EXPIRED` (from `HITL/ACTION_PENDING/ACTIONED`), and `VERIFIED` (from `ACTIONED`).

Therefore, **Option A’s initial producer is accepted; the subsequent transition-wrapper is left open pending a separate contract decision** (a distinct small ADR 011, or an extension to this one with evidence). ADR 010 does **not** authorize any mutation beyond `VALIDATING → EXCEPTION`, and any implementation that adds state semantics not listed here must stop and report BLOCKED/INCOMPLETE.

Conclusion: ADR 010 was designed to avoid inventing an alternative parking state because the repository already has `EXCEPTION` and a legal `VALIDATING → EXCEPTION` edge; Option B cannot be supported from current evidence.

## Evidence and alternatives

**Eliminating Option B (explicit new state):**
- State vocab already has a legal, semantically consistent `EXCEPTION` state (see `claims/statemachine.go`, ADR-007 parser-policy, ADR-009 low-evidence HITL via R8).
- A query helper `ListPendingHITL` intentionally returns `HITL` ∪ `ACTION_PENDING`, not a separate parking row; adding parking would change the query contract and indexes.
- No product requirement from the APA-91 issue justifies a distinct state: the gap is absent producer, not impossible representation.
- Adding state implies transition API/docs/workflow changes not otherwise authored. rejected.

**Evidence retained:**
- `sufficiency.go:64-68`: Routing constants.
- `sufficiency.go:204`: `res.Routing = RoutingHITL` on insufficient.
- `sufficiency.go:241-243`: low-OCR path `RoutingContinue`/`RoutingHITL`.
- `processor.go:886–901`: evaluation and synthesis.
- `processor.go:1000–1020`: envelope/launch path (post-build), best-effort.
- `claims/statemachine.go` (`Transitions`, `CanTransition`, `Transition`, `CodeIdempotentReplay`, tenant/version checks).
- `repository/postgres/hitl.go` query composition.
- `handlers/decision.go` (all).
- `workflows/claim-investigation.yaml` callback/decision apply.

## Implementation gates (binding)

1. Implement only the first transition; all mid-chain progression must be added in a separate, evidenced contract.
2. RED-first E2E test must demonstrate claim transition → verify directly (DB status row) and the test must fail when the producer is removed (negative control).
3. Required negative controls: sufficient evidence no row mutation/audit/event, wrong-tenant no read, unauth/unauthz no decision rows, version conflict block, idempotent replay no duplicate, transaction failure rollback, audit row matches row transition, no orphan GCW launch on failed transaction.
4. No release qualification, no merge, no deployment: ADR 010 must reach `Accepted` and integration PR exists (still no merge) before code.
5. Decision to extend the workflow mid-chain remains open: require a separate ADR 011 before any `EXCEPTION → INVESTIGATION_REQUIRED` producer.

## Consequences

- Fastens the missing producer gap to an existing, legal, replayable state transition.
- Removes any justification for state-vocabulary redesign.
- Explicitly records that the human-review serial progression ownership is unresolved, protecting against speculative workflow/decision changes.
- Does not close APA-91; implementation next, with a separate edge decision gate.
- Releases cannot claim that a human-review path is auto-complete; only `VALIDATING → EXCEPTION` is authorized.