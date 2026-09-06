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
