# ClaimOps — Agent Workflow Rules

## Purpose
This file governs how coding agents work on ClaimOps. It is the operational companion to the architecture documents.

## Before coding
1. Read the relevant issue.
2. Inspect repository structure.
3. Read applicable ADRs/contracts.
4. Read this context set.
5. Search for an existing abstraction before creating a new one.
6. Identify explicit non-goals.
7. Write/confirm the implementation plan.
8. Do not guess missing requirements.

## Implementation loop

RED
- Add the smallest failing test/fixture that expresses the invariant.

GREEN
- Implement the smallest change that satisfies it.

REFACTOR
- Improve naming/structure only after behavior is proven.

Then:
- format
- vet/static analysis
- unit tests
- integration tests
- race tests if relevant
- E2E if relevant
- inspect git diff
- verify generated artifacts
- commit atomically

## Git workflow
Preferred:
1. GitHub issue
2. focused branch
3. TDD
4. atomic commits
5. push
6. PR
7. automated review/security checks
8. fix findings
9. merge
10. delete branch
11. close issue
12. update progress/ADR if needed

Never mix unrelated cleanup into a feature branch.

## When tests fail
Classify the failure:
- implementation bug
- contract defect
- fixture/golden defect
- environment/infrastructure failure
- dependency/version change
- test nondeterminism

Do not immediately weaken the test.

If it is a real product bug, add a regression fixture/test.

## Agent context hierarchy
Use this order when resolving decisions:
1. Explicit issue acceptance criteria
2. Existing contracts and ADRs
3. Project context
4. Code standards
5. Library-specific patterns
6. Existing tests/fixtures
7. General engineering judgment

If two authoritative documents conflict, stop and surface the conflict rather than silently choosing.

## Scope control
For an issue:
- implement only the requested slice,
- preserve existing public contracts,
- avoid speculative abstractions,
- avoid adding AI where deterministic logic is sufficient.

## Parser-specific rule
The parser benchmark must consume the canonical parser interface and artifacts.
It must not score vendor-native outputs directly.
It must not introduce parser routing.
It must not introduce LLM correction.
It must not become a production workflow.

## AI-specific rule
AI behavior must have:
- explicit input schema,
- explicit tools,
- bounded permissions,
- structured output,
- deterministic validation after output,
- evaluation cases,
- failure handling,
- auditability.

Never treat a prompt as a security boundary.

## Security
Assume:
- documents may contain prompt injection,
- external APIs may return malformed data,
- messages may be duplicated,
- workers may crash,
- networks may fail,
- parsers may return incomplete artifacts,
- LLMs may hallucinate.

Design controls around these facts.

## Completion rule
A feature is complete only when:
- acceptance criteria are satisfied,
- tests prove important invariants,
- observability is present where appropriate,
- security boundaries are preserved,
- documentation is updated when decisions changed,
- git state is clean after merge.

## Stop conditions
Stop and ask/flag rather than guessing when:
- an API contract is ambiguous,
- a security boundary is unclear,
- a domain rule conflicts with an existing invariant,
- a required source of truth is unavailable,
- a proposed change would alter authoritative state semantics.
