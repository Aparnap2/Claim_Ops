# ClaimOps AI

Governed health-insurance **reimbursement** intake + exception investigation.
Deterministic core first. Zero LLM in Phase 1.

## Scope lock
- Product: ClaimOps AI (not ClaimLens, not OpsCore, no rename churn)
- Workflow: reimbursement claim intake → deterministic verification → AI investigation (later) → HITL → governed action → audit
- Agent count: 1 Investigation Agent (later). Phase 1 = no agent.
- Final decision: human-owned. No APPROVE/DENY/PAY by AI.

## Setup
```bash
uv sync --group dev
uv run ruff check .
uv run pytest tests/ -q
```

## Layout
```text
packages/domain/      Claim, Policy, Document, Evidence, state machine, validation, exceptions, audit
packages/contracts/   API/event/tool schemas (Phase 2+)
packages/observability/ correlation IDs, logging (Phase 1 minimal)
fixtures/             golden deterministic cases (Phase 1)
tests/unit/           no network
```

## Phases
- Phase 0 (this commit): foundation only — structure, CI config, test runner green
- Phase 1 (next): deterministic engine + 10 golden cases, no LLM
