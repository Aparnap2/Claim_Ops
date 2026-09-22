# ADR 008 — Agent Authoritative Mutation Boundary

* Status: Accepted 2026-09-22 (APA-11)
* Base: `main@1ad00dd` after APA-9 merge
* Supercedes: implicit assumption; makes explicit what was already enforced

## Context

APA-11 audits: "DocumentAgent/VendorAgent/… hold DBProvider and call
authoritative Upsert/Update; LLM output does not directly set status but the
agent that invokes the LLM is also the writer." The question is not
"does every DBProvider use need removal" but "which writes are
authoritative and can model-derived data reach them without a
deterministic gate?"

Read/evidence access may remain agent-owned. Authoritative mutation is
the security boundary.

## Decision

**Agents are read-only over authoritative state.** Deterministic code
owns truth, state, control, validation, and side effects (Decision Log,
Agent Operating Contract rule 2).

* **Authoritative tables** (claim, claim_events, documents,
  document storage, HITL/claim status transitions): mutated only via
  deterministic, tenant-scoped, version-checked commands:
  `postgres.Repository.SaveClaim` (`INSERT … ON CONFLICT … WHERE version`),
  `AppendEvent`, `InsertDocument`/`InsertEvidence` via
  `StoreBridge`/`worker.Processor`, and the version-checked `UPDATE claims`
  in `handlers/decision.go`. All are called from deterministic ingress or
  workers, never from the cognitive loop, and never with model-derived
  arguments except via a validated report that itself is still evidence.

* **Evidence tables** (investigations envelope, evidence/field_evidence,
  audit_log, investigation_reports as write-once store): agents may
  `LoadEnvelope` (RLS + tenant echo) and read via `PGReaders`
  (`LoadClaim`, `ListDocuments`, `ListEvidence`, `SearchEvidence`) and
  append audit rows via `AppendToolCall`. The sole writer `T11`
  (`create_investigation_report` / `tools.Report` → `PGReportStore.Insert`)
  is **intentionally denied to the model path**: `orchestrate.Loop`
  escalates any `ToolCreateInvestigationReport` act as
  `INVALID_OUTPUT`/`ErrToolDenied` before any `Executor.Execute` (see
  `apps/api/internal/investigate/orchestrate/loop.go:646`). Report
  persistence, if needed, occurs deterministically *outside* the loop after
  `ValidateInvestigationOutput` + `CheckReportGrounding`.

* **Gate between model and authoritative write**:
  `Request.Validate` → `Scope.Validate` → allowlist (`invest.IsAllowlisted`)
  → `Scope.IsAllowed` → `Executor` budget/deadline/tenant echo →
  `Loop` denial of `T11` → `Response.Validate` → `Loop` grounding on
  `KnownEvidence` (IDs grown only from validated `Response.IDs`). Invalid,
  undeclared, hallucinated, or oversized model output fails closed with
  typed `ErrModelContract`/`ErrToolDenied`/`ErrGrounding` and typed
  `Escalation*` reasons; `Executor.Calls()` proves no backend was touched.

## Inventory (reachable from agent code at `1ad00dd`)

| Table | Writer | Reachable from `cmd/agent`? | Class |
|-------|--------|-----------------------------|-------|
| `claims` | `Repository.SaveClaim`, `handlers.Decision UPDATE` | No — `cmd/agent` registry has no writer; `PGReaders.LoadClaim` is read-only projection | Authoritative |
| `claim_events` | `Repository.AppendEvent` | No | Authoritative |
| `documents` | `Repository.InsertDocument` / `StoreBridge` | No | Authoritative |
| `field_evidence`/`evidence` | `Processor.InsertEvidence` | No | Authoritative (structured evidence) |
| `investigations` | `PGEnvelopeStore.SaveEnvelope` | No — agent only `LoadEnvelope` (prod) or inline envelope in `APP_ENV=local` test path | Evidence (write-once envelope) |
| `investigation_reports` | `PGReportStore.Insert` via `T11` | No — `T11` denied in `Loop`; not wired in `cmd/agent` registry | Evidence (write-once, hash-verified) |
| `audit_log` | `investigate.AppendToolCall` via `Executor` hook | Yes — best-effort hook, IDs/hashes/counts only, never fails tool result | Audit (append-only, not authoritative claim state) |

No authoritative `INSERT`/`UPDATE`/`Upsert` is reachable from
`apps/api/cmd/agent`'s tool registry
(`ToolGetClaim`, `ToolGetDocuments`, `ToolGetEvidence`,
`ToolSearchEvidence`, `ToolGetVerificationFindings`) and none from
`investigate.ToolFunc` dispatch when invoked via the loop.

## Consequences

* No business-behavior change. The loop's `T11` denial, executor allowlist,
  and publisher/reader split already enforce the invariant; this ADR makes
  the invariant contractual and adds regression tests that fail if the
  boundary is widened.
* Future agents that need to persist must go through a deterministic
  `internal/claims` or `internal/documents` command (version-checked,
  tenant-scoped). Adding `ToolCreateInvestigationReport` to
  `cmd/agent`'s registry or removing the `Loop` denial must be a
  deliberate ADR change with updated tests, not a silent wiring edit.
* Evidence: `apps/api/internal/investigate/orchestrate/mutation_boundary_test.go`
  (registry read-only, loop `T11` denial, report cannot carry `claim_status`
  transition), existing `guardrail_test.go` (`T11` never callable, output
  never mutates claim), and `PGReaders` read-only projection.

## Alternatives considered

* Remove `DBProvider`/pool from agents entirely — rejected per execution
  contract: distinguish read/evidence from authoritative write; tightening
  to pure read-only at the pool level would not add security beyond the
  tool/loop gates and would complicate legitimate audit/envelope reads.
* Introduce a separate `CommandService` interface for every write now —
  deferred: the existing repositories already are version-checked commands;
  a new abstraction would add indirection without changing the reachable
  set. Revisit if a second authoritative writer becomes agent-adjacent.
