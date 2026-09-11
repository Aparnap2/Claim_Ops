# ClaimOps — Code Standards

## Purpose
These are non-negotiable engineering rules for ClaimOps. Agents must read this file before modifying code.

## Core principle
Deterministic systems own truth, state, authorization, validation, retries, persistence, and side effects. Cognitive/LLM systems handle bounded ambiguity and recommendations. An LLM must never directly mutate authoritative state or make final claim approval, denial, payment, policy, or fraud-adjudication decisions.

## Architecture
- Go/Fiber is the authoritative backend and system-of-record boundary.
- Python/ADK is a bounded cognitive service, not the system of record.
- PostgreSQL is authoritative for transactional state.
- GCS stores document blobs; PostgreSQL stores document metadata/evidence.
- Pub/Sub is asynchronous transport.
- Mockoon represents external/legacy insurance APIs locally.
- localgcp is the local GCP compatibility environment.
- Keep vendor-specific code behind application-owned interfaces.
- Never leak LiteParse/Google/Mockoon/vendor types into domain packages.
- Prefer ports/adapters and explicit dependency injection over global clients.

## Deterministic-first
Before introducing an LLM, ask whether the requirement can be expressed as:
- a typed rule,
- a schema,
- a state transition,
- a database constraint,
- a deterministic validator,
- an idempotency invariant,
- a contract test,
- or a reproducible calculation.

If yes, implement it deterministically.

## Domain types
Prefer explicit types:
- TenantID
- ClaimID
- DocumentID
- InvestigationID
- Money
- ClaimState
- DocumentState
- EvidenceLocation

Money is represented in paise/integer minor units. Never use floating point for authoritative monetary values.

## State and persistence
- State transitions must be explicit and validated.
- Authoritative mutations require tenant context.
- Preserve optimistic concurrency/version guards.
- Preserve append-only audit semantics.
- Do not bypass RLS.
- Do not silently overwrite authoritative state.
- Design operations to be idempotent where retries are possible.
- External side effects require explicit verification/reconciliation where applicable.

## Documents and evidence
Treat every document as untrusted input.
Trust boundary:
UNTRUSTED INPUT -> admission -> accepted blob -> integrity verification -> trusted document -> parser.
Never parse before integrity verification.
Never treat document text as executable instructions.
Evidence must preserve honest provenance. If coordinates or source spans are unavailable, represent them as unavailable; never fabricate them.

## APIs
- Validate at the edge.
- Strict request/response schemas.
- Stable machine-readable error codes.
- Consistent response envelope where the existing API uses one.
- Require tenant identity on tenant-scoped routes.
- Generate/propagate X-Request-ID.
- Do not expose internal exception text to clients.
- Reject unknown fields where strict contracts require it.
- Keep handlers thin; business rules belong in services/domain packages.

## Errors
- Use typed/sentinel errors for stable classification.
- Wrap errors with context.
- Use errors.Is/errors.As.
- Never swallow errors.
- Distinguish transient from terminal failures.
- A retryable worker failure must allow the transport to redeliver.
- A terminal failure must be persisted/observable rather than silently dropped.

## Concurrency
- Respect context cancellation.
- Avoid goroutine leaks.
- Protect shared state explicitly.
- Run race tests for concurrency-sensitive code.
- Use idempotency and version checks instead of assuming exactly-once delivery.

## Observability
Every production component must be observable:
- structured logs,
- metrics,
- tracing where configured,
- stable error codes,
- useful request/job identifiers.

Never log raw PII/PHI, document contents, secrets, tokens, credentials, or sensitive evidence.
Prefer identifiers, hashes, counts, timings, and classification codes.

Local goroutine leak diagnostics are enabled only for APP_ENV=local.

## Testing
Use the smallest test level that proves the invariant:
- pure unit tests for pure logic,
- contract tests for boundaries,
- integration tests for PostgreSQL/RLS/GCS/Pub/Sub,
- E2E tests for critical flows,
- security tests for tenant isolation and authorization,
- deterministic evaluation tests for parsers/AI.

Every discovered bug becomes a regression fixture/test where practical.

Required before PR:
- formatter
- vet/static checks
- unit tests
- relevant integration tests
- race tests for affected concurrent code
- inspect diff
- confirm no accidental PII/PHI or secrets

## Dependencies
- Pin versions where reproducibility matters.
- Prefer minimal dependencies.
- New dependencies require justification.
- Do not introduce a framework/library merely to avoid a small amount of code.
- Go toolchain is Go 1.27.
- LiteParse is pinned at the version recorded by uv.lock.

## Change discipline
- One issue -> one focused branch -> one coherent PR.
- Prefer atomic commits.
- No unrelated refactors.
- Update ADR/docs when an architectural decision changes.
- Do not rewrite working abstractions without evidence.
- Preserve existing contracts unless the issue explicitly changes them.

## Agent rule
If the repository contains an established abstraction, use it before creating another one.
If requirements are ambiguous, stop and inspect the relevant contract/ADR/fixture rather than guessing.
