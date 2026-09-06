# Agent Operating Contract (lock v1)

1. Reimbursement MVP only. No cashless pre-auth, no payout execution.
2. Hierarchy: deterministic code > structured models > rules > retrieval > LLM. No LLM for arithmetic, state, auth, money, policy enforcement, DB mutation, retry, idempotency.
3. `packages/domain/` must compile/test without `ai/`. No `ai/` in Phase 0/1.
4. Every entity carries `tenant_id`. Tools scoped to `tenant_id + claim_id`.
5. State transitions via `transition()` only: RECEIVED → REGISTERED → DOCUMENTS_RECEIVED → VALIDATING → READY_FOR_REVIEW | EXCEPTION → INVESTIGATION_REQUIRED → WAITING_FOR_EVIDENCE → HITL → ACTION_PENDING → ACTIONED → VERIFIED → CLOSED. Idempotent on `(claim_id, event_id)`, optimistic `version` check.
6. Amounts = `Decimal` quantized 2dp, INR. Dates = `date`, `admission <= discharge`.
7. Exception taxonomy A–E fixed; every failure classified TRANSIENT/PERMANENT/VALIDATION/MODEL/EXTRACTION/EXTERNAL/AUTH/UNKNOWN. Retry transient only, max 3, then DLQ/terminal.
8. Every bug → fixture + test + classification. No fix without regression.
9. Commits: `feat(domain): ...`, `test(domain): ...`. One concern per commit. No direct-to-main after init.
10. Done = implementation + tests pass + acceptance criteria + audit event + docs updated.
11. Every new component is born observable, typed, testable, replaceable: structured logs with request/correlation/tenant/claim IDs (never raw docs, secrets, PII — IDs and hashes only), stable metric names (`internal/metrics` vocabulary; renames need ADR), versioned event contracts (`<event>.vN`), narrow repository ports, explicit transaction boundaries. No hardening later.
12. Dev loop per change: status → issue → acceptance criteria → inspect → failing test → small implementation → targeted tests → full gates (`make check`) → diff review → commit atomically → push → PR.
13. Before declaring done, answer: timeout? called twice? state changed underneath? wrong tenant? malformed external data? LLM nonsense (future)? Log the answers or the handling code.
14. Reject: TODO without issue link, dead code, `utils.go` dumps, untyped service boundaries, magic constants, temporary bypasses without an issue.
