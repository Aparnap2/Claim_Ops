# ClaimOps AI

Governed health-insurance **reimbursement** intake + exception investigation.
Deterministic systems first; LLM only where semantic reasoning is required.

## Scope lock
- Product: ClaimOps AI (not ClaimLens, not OpsCore, no rename churn)
- Workflow: reimbursement intake → deterministic verification → AI investigation → HITL → governed action → audit
- **Go is the authoritative backend** (edge, lifecycle, validation, tenancy, audit).
  Python is the bounded cognitive service + specification artifacts (ADR-001).
- Agent count: 1 Investigation Agent (Phase 6). No APPROVE/DENY/PAY by AI, ever.
- LLM provider: Groq (`openai/gpt-oss-20b`, Qwen for specialist calls), key via
  environment only, stub-until-key discipline (ADR-002).

## Prerequisites
- Go 1.25+, Python 3.12 + `uv`, Docker (Postgres, Mockoon)

## Setup
```bash
# Python spec suite (no network)
uv sync --group dev
uv run ruff check .
uv run pytest tests/ -q

# Go backend
cd apps/api && go build ./... && go test ./...

# Local services (one at a time, never compose for dev)
docker start claimops-postgres      # postgres:16 on :5433
docker start claimops-mockoon       # mocked externals on :3001
# Postgres integration tests need a live DB:
TEST_POSTGRES_DSN=postgres://claimops_app:claimops_app@localhost:5433/claimops \
  go test ./internal/repository/postgres/ -count=1
# External contract tests need Mockoon:
go test ./internal/adapters/http/ -count=1
```

## Layout
```text
apps/api/               Go Fiber edge + deterministic core (authoritative)
  cmd/api/              main (wiring lives in internal/app for testability)
  internal/
    claims/             Claim aggregate + state machine (stdlib-only)
    validation/         six pure validators (stdlib-only)
    workflow/           tenant-scoped store + Apply + domain events
    ports/              Policy/Claims/Provider/Risk outbound interfaces
    adapters/http/      typed clients (timeouts, request IDs, strict decode)
    contracts/external/ wire DTOs (aliases of ports types)
    repository/postgres pgx repo, txn-local tenant context, RLS
    evidence/           source-grounded evidence model
    middleware/         request ID, tenant gate; handlers/; config/; observability/
packages/domain/        Python specification artifacts (Pydantic strict)
fixtures/               golden_cases.json — language-independent behavior contract
mocks/mockoon/          claims-systems.json — 4 systems × 7 scenarios
infra/postgres/         migrations 001 (schema+RLS) 002 (grants) 003 (evidence)
tests/unit/             Python golden suite (no network, untouched by Go work)
docs/adr/               001 go-first, 002 groq provider, 004 external boundary
```

## Phase progression (one reviewable PR each)
- ✅ PR #1 — repo foundation · ✅ PR #2 — Go system edge
- ✅ PR #3 — deterministic domain core · ✅ PR #4 — LLM provider decision
- ✅ PR #6 — Postgres + RLS tenant isolation
- ⏳ PR #7 — external contracts + Mockoon (this branch)
- Next: document ingestion → deterministic extraction → exception corpus →
  cognitive investigation agent → HITL → evals

## Standing rules
- Deterministic > structured > rules > retrieval > LLM. No LLM for
  arithmetic, state, auth, money, policy, mutation, retry, idempotency.
- Every entity carries `tenant_id`; RLS enforced in Postgres (FORCE),
  transaction-local `set_config` on pooled connections.
- Every bug → fixture + test + failure classification.
- Atomic commits; no direct-to-main; docs live with the change.
