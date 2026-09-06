# ADR-001: Go-first system edge, Python as bounded cognitive service

Date: 2026-09-06
Status: accepted

## Context

The scaffold drifted toward a Python monolith: `packages/domain/` was
becoming the system of record for claim lifecycle, validation, tenancy,
and audit. The agreed architecture is Go at the application boundary.

## Decision

- `apps/api` (Go + Fiber) is authoritative for: REST edge, auth/RBAC,
  tenant context, claim lifecycle, deterministic validation, workflow
  commands, idempotency, concurrency control, HITL actions, audit emission.
- `packages/domain` (Python) is demoted to specification artifacts +
  the future bounded AI service (`POST /investigate` only; no state
  mutation, no approve/deny/pay).
- `fixtures/golden_cases.json` is the language-independent behavioral
  contract both implementations must satisfy.

## Consequences

- Deterministic core must be re-implemented in
  `apps/api/internal/{claims,validation,workflow}` in a later phase.
- Python tests remain green and serve as the spec until Go parity lands.
