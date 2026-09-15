# Rationale — Phase 3 E2E Qualification

## Context

Phase 2 left worker carrying Parser, Extractor registry, and Scope seams via BuildFullProcessor, with cmd/api and cmd/worker both wired. Localgcp and Mockoon already gate integration tests. The remaining gap is the durable workflow and cognitive service boundary: worker currently ends at invest.Build envelope bytes with no workflow to carry investigation to HITL.

## Options considered

### Workflow location

- In worker Go code: simple, no emulator, but not durable across restarts and not how production Workflows behaves.
- In API: couples API to long running investigation.
- In GCW emulator: matches production, durable, callback based HITL, requires emulator.

Selected GCW emulator.

### Agent packaging

- Library inside worker: no service boundary, no independent scaling.
- Separate Agent service (selected): matches Cloud Run Agent, isolates cognitive loop, allows independent ModelClient config, requires HTTP boundary.

### Model provider

- Direct Groq SDK import in orchestrator: leaks provider into control loop.
- ModelClient seam with Groq HTTP client (selected): preserves provider neutrality, testable with Mock script.

### Network

- localhost everywhere: breaks inside containers.
- Docker network with service names (selected): works locally and in containers, respects no compose rule via docker run --network.

## References

- GCW emulator: ghcr.io/lemonberrylabs/gcw-emulator v0.5.0, ports 8787/8788, REST paths /v1/projects/{project}/locations/{location}/workflows
- ModelClient seam: internal/investigate/orchestrate/model.go
- Frozen contracts: docs/specs 01, 07, 08, 09, prd.md
- Local parity: localgcp, Mockoon, Cloud SQL

