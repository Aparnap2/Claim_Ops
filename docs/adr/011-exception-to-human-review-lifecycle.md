# ADR 011 — Ownership and Authorization of the Exception-to-Human-Review Lifecycle

* Status: Draft (design artifact, not implemented)
* Base: `main@0f33724ddc0c4ed0cc04292a93100ec2b495946f` (recorded baseline; current HEAD reverified at creation)
* Related: ADR 010 (Accepted, limited scope), APA-91 (High, open), `internal/claims/statemachine.go`, `workflows/claim-investigation.yaml`, `internal/handlers/decision.go`
* Scope: contract only. No code, no state-machine edits, no workflow YAML changes.

## Context

ADR 010 authorized exactly one durable transition: `VALIDATING → EXCEPTION`, produced
by `worker.Processor.runNewPipeline`. It deliberately left unresolved the ownership,
authorization, idempotency, audit/outbox behavior, retries, callbacks, expiry and
recovery for the downstream human-review chain:

`EXCEPTION → INVESTIGATION_REQUIRED → WAITING_FOR_EVIDENCE → HITL → ACTION_PENDING → (ACTIONED | EXPIRED)`

Repository evidence (ADR 010) shows that today:

- `DecisionHandler` is the only runtime mutation entry point, with an action map
  `APPROVE→VERIFIED`, `REJECT→CLOSED`, `ESCALATE→EXCEPTION`, `HITL/HOLD→HITL`,
  `EXPIRE→EXPIRED`.
- No production command creates `INVESTIGATION_REQUIRED` or `WAITING_FOR_EVIDENCE`.
- `HITL → ACTION_PENDING` is entered only by an operator calling `/decision` with
  `HITL`/`HOLD`, unless the investigate callback/apply step is modified.
- `ACTION_PENDING → ACTIONED` is not reachable from the public decision API.
- The GCW workflow's `apply` step can only forward `APPROVE/REJECT/ESCALATE/HITL/HOLD/EXPIRE`.

This ADR decides who may drive each edge, by what authorization, with what
idempotency and audit guarantees, and how timeouts/retries/crash recovery behave.

## Decision (proposed)

### Ownership matrix

| Edge | Owner | Trigger | Authorization |
|---|---|---|---|
| `EXCEPTION → INVESTIGATION_REQUIRED` | Go deterministic worker (post-exception orchestrator) OR GCW workflow step | Investigation started / evidence collection kicked off | Service account / worker identity; no human action required |
| `INVESTIGATION_REQUIRED → WAITING_FOR_EVIDENCE` | Go deterministic worker | Missing evidence enumerated (`MissingEvidence`) | Service account / worker identity |
| `WAITING_FOR_EVIDENCE → HITL` | Go deterministic worker (auto) OR human `ESCALATE` | Sufficiency gate routes to HITL (R8) or operator escalation | Service account for auto; operator for manual |
| `HITL → ACTION_PENDING` | Human operator via `/decision` (`HITL`/`HOLD`) | Operator review decision | Authenticated operator with tenant scope |
| `ACTION_PENDING → ACTIONED` | Human operator via `/decision` (`APPROVE`) | Operator approval | Authenticated operator with tenant scope |
| `HITL/ACTION_PENDING/ACTIONED → EXPIRED` | System (timeout) OR operator `EXPIRE` | Deadline exceeded | System timer (service account) or operator |
| Terminal `VERIFIED`/`CLOSED` | Human operator (`APPROVE`/`REJECT`) | Final operator decision | Authenticated operator with tenant scope |

### Authorization rules

1. **No LLM/agent may mutate claim state.** The cognitive loop remains read-only
   (ADR 008). Only the deterministic worker and authenticated operators may transition.
2. **Tenant scope is mandatory on every edge.** `BeginTenantTx` + RLS; cross-tenant
   returns not-found/terminal failure, never a state change.
3. **Version CAS on every edge.** `WHERE claims.version = $old`; `ErrVersionConflict`
   on loss; redelivery retries against the new version.
4. **Idempotency.** Event ID deterministic per `(tenant, claim, edge, trigger-identity)`;
   replay yields `CodeIdempotentReplay`, no new rows, no new audit.
5. **Audit.** Every edge writes one `audit_log` row with `actor`, `action=state_transition`,
   `entity=claim`, before/after status+version. No partial writes (transaction boundary).
6. **Outbox.** State transitions that require external notification (HITL creation,
   expiry) emit outbox events within the same transaction; dispatch is best-effort
   with bounded retries (APA-44).
7. **Crash recovery.** Redelivery re-enters the producer; idempotent replay prevents
   duplicate transitions. Orphan GCW launches are reconciled via `EnsureLaunched`
   idempotency (APA-39), never by rolling back the claim.
8. **No speculative auto-approval.** The LLM cannot reach `APPROVE`/`VERIFIED`/
   `CLOSED`/`ACTIONED` by any path. Only authenticated operators may.

### Action mapping decision (open sub-question)

The GCW workflow's `apply` step can only forward `APPROVE/REJECT/ESCALATE/HITL/HOLD/EXPIRE`.
Two options:

- **Option A (recommended):** Extend the decision endpoint's action map to accept a
  `PROGRESS`/`ADVANCE` action whose target is derived server-side from current status
  (e.g. `EXCEPTION → INVESTIGATION_REQUIRED`). This keeps authorization in Go, not
  in the workflow YAML.
- **Option B:** Reuse `ESCALATE` for `EXCEPTION → INVESTIGATION_REQUIRED`. Rejected:
  `ESCALATE` already means `VALIDATING → EXCEPTION`; overloading it conflates two
  distinct product events and breaks audit clarity.

This sub-question is recorded but not resolved here; it needs a separate small decision
once the ownership matrix is approved.

### Consequences

- Unblocks ADR 011 approval → enables a follow-on PR implementing the first
  downstream edge (`EXCEPTION → INVESTIGATION_REQUIRED`) with its own RED-first tests.
- Keeps ADR 010's scope intact; no change to `VALIDATING → EXCEPTION`.
- Does not close APA-91; full human-review lifecycle remains a separate capability.

### Implementation gates (binding, before any code)

1. This ADR reaches `Accepted`.
2. A RED-first E2E test per authorized edge, with negative controls for forbidden edges.
3. No LLM/agent path reaches `APPROVE`/`VERIFIED`/`CLOSED`/`ACTIONED`.
4. PR #162 (initial producer) merged to `main` first, so downstream edges build on
   the durable `EXCEPTION` state.
