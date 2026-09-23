# 10 — Phase 3 Local Cloud E2E Qualification

**Status**: Partially implemented (slices 1–2 landed #80 main@5cb15f1; remainder pending)
**Date**: 2026-09-15
**Depends on**: Phase 2 frozen baseline (PR #79, SHA eec02a0); current base main@9f7a196 (APA-13)

## Summary

Qualify the final production shaped architecture locally without redesigning frozen contracts. Prove the real application traverses POST claim to REPORT_READY and ESCALATED through authoritative DB, GCS, outbox, Pub/Sub, worker, parser, extraction, assembly, verification, exception, durable workflow, agent service, investigation orchestrator, real tools with RLS, ModelClient, grounding, and HITL callback. MockModelClient only is mocked initially, then Groq qualifies the same path.

## Requirements

### Frozen (must not change)

eval v1 corpus A through P, E0 through E3 definitions and expectations, investigation capability contract, epistemic contract, R1 through R10, ID domains, parser contract, extraction contract, assembly semantics, verification semantics, orchestrator protocol, AgreedRaw boundary, encoding/json v1, authoritative state rules. No ADK, LangGraph, another agent framework, direct model mutation, arbitrary SQL or HTTP, new exception taxonomy, new OutcomeKind, fabricated provenance, confidence floats, or speculative repo cleanup.

### Topology to reproduce

API on Cloud Run, Agent on Cloud Run, Worker on Compute Engine, Cloud SQL, GCS, Pub/Sub, GCW, Mockoon, ModelClient seam. Locally: Go API, Go Agent :8081, Go Worker separate process, localgcp Cloud SQL :5434 GCS emulator Pub/Sub emulator, GCW emulator 8787/8788, Mockoon :3001.

Production mapping table per mission prompt is authoritative.

### Workflow

Must represent business state not agent internals: claim received, document processing completed, exception detected, start investigation, wait for result, REPORT_READY vs ESCALATED branch, human decision callback, authoritative Go command. Agent internal loop stays inside Agent service.

### Agent service

POST /v1/investigations adapter around existing investigate/orchestrate code. Authenticate, enforce tenant, accept investigation context, invoke Go orchestrator, persist via existing state, return existing semantics, never directly mutate authoritative claim state from model output.

### ModelClient

Preserve ModelClient as only inference boundary. Provide MockModelClient (scripted deterministic, CALL_TOOL and SUBMIT_REPORT, real orchestrator/executor/tools, all gates enforced) and GroqModelClient (OpenAI compatible HTTP, orchestrator agnostic). Budgets, grounding, repetition, tenant checks remain enforced.

### Paths to prove

Happy path trace from API through every boundary to REPORT_READY with MockModelClient. Second path to ESCALATED via legitimate deterministic budget or deadline or repetition or no progress. HITL callback: workflow waits, simulated human callback, workflow resumes, Go authoritative command, tenant/authorization verified, agent does not perform final mutation.

### Failure matrices

Infrastructure: duplicate PubSub safe convergence, worker restart convergence, localgcp restart recovery, corrupt GCS integrity mismatch terminal, PubSub unavailable transient, Mockoon unavailable upstream semantics. Cognitive via real orchestrator: malformed response bounded re-prompt, undeclared capability rejected, fabricated evidence grounding rejection, cross tenant rejected, repetition bounded, deadline escalation, turn and call budget exhaustion.

### Groq qualification

Only after Mock control path green. GroqModelClient changes only ModelClient implementation. Compare Fake vs Groq on same frozen harness with hard gates fabricated=0 crossTenant=0 unauthorized=0. No eval mutation.

### Observability and verification

Correlation across HTTP request to report via existing request IDs. No PII/PHI, secrets, document contents in logs. Commands: gofmt, go vet, go test, go test -race where practical, plus frozen eval. git diff on eval and tests must be empty before and after.

## Decision

### Architecture options considered

**Option A: Embedded workflow (no GCW).** Workflow logic in Go code inside worker or API, triggered directly. Pros: zero emulator, trivial local. Cons: does not reproduce GCW durable wait and callback, diverges from production, hides workflow contract risk, HITL wait not proven.

**Option B: GCW emulator as durable coordinator (selected).** GCW emulator container provides REST and gRPC on 8787/8788, workflow YAML stored in repo, API or worker starts execution via REST, GCW calls Agent service via http.call steps, Agent invokes Loop, GCW waits via callbacks for HITL. Pros: reproduces production responsibilities, proves WorkflowProvider boundary, exercises real callback durability, keeps agent loop inside Agent. Cons: emulator container and network wiring.

**Selected: Option B** because the mission requires proving Workflows equals durable business coordination and that callback durability is not collapsed into application code for test convenience.

### Topology implementation (repo rule aware)

Repo rule forbids docker compose. Use docker run with shared network claimops-net to get service name resolution without compose file.

Network: claimops-net (bridge).

Containers (docker run, --network claimops-net, --rm where ephemeral):

- claimops-postgres :5433 (existing)
- localgcp :8085 PubSub :4443 Storage :5434 Cloud SQL proxy
- claimops-mockoon :3001
- gcw-emulator :8787 HTTP :8788 gRPC (image ghcr.io/lemonberrylabs/gcw-emulator:latest v0.5.0, env WORKFLOWS_DIR=/workflows, PROJECT=my-project LOCATION=us-central1, mount repo workflows dir)

Service names resolvable: api, agent, worker, gcw, localgcp, mockoon. No localhost assumption inside containers.

Local dev without Docker network uses localhost with *_EMULATOR_HOST env.

### WorkflowProvider boundary

New interface in internal/workflow/provider.go:

```
WorkflowProvider
  StartExecution(ctx, workflowID string, argument any) (executionName string, err error)
  GetExecution(ctx, executionName string) (state, result, err error)
  SendCallback(ctx, callbackID string, payload any) error
  DeployWorkflow(ctx, workflowID string, sourceContents string) error
```

Implementations: GCWProvider (REST client to gcw:8787 or localhost:8787 via WORKFLOWS_EMULATOR_HOST, httpx with timeout, no SDK import) and NoopProvider (for tests without emulator). No GCW types leak into domain.

Production code depends only on interface. Config selects provider via WORKFLOWS_EMULATOR_HOST presence.

### Workflow definition

File workflows/claim-investigation.yaml, business state only:

```
main:
  params: [args]
  steps:
    - init:
        assign:
          - claim_id: ${args.claim_id}
          - tenant_id: ${args.tenant_id}
          - investigation_id: ${args.investigation_id}
    - investigate:
        call: http.post
        args:
          url: ${sys.get_env("AGENT_URL") + "/v1/investigations"}
          body:
            investigation_id: ${investigation_id}
            tenant_id: ${tenant_id}
            claim_id: ${claim_id}
        result: invResult
    - check:
        switch:
          - condition: ${invResult.body.outcome == "REPORT_READY"}
            next: await_human
          - condition: true
            next: await_human_escalated
    - await_human:
        call: events.create_callback_endpoint
        args:
          callback_type: "hitl-decision"
        result: cb
    - wait_human:
        call: events.await_callback
        args:
          callback: ${cb.callback}
          timeout: 3600
        result: decision
    - apply:
        call: http.post
        args:
          url: ${sys.get_env("API_URL") + "/v1/claims/" + claim_id + "/decision"}
          headers:
            X-Tenant-ID: ${tenant_id}
          body: ${decision}
        result: applied
    - done:
        return: ${applied.body}
```

Agent loop not represented as workflow steps. Callback timeout 1h matches MaxDeadlineMs.

Variants await_human and await_human_escalated converge to same HITL pattern; branch exists for observability only.

### Agent service

New cmd/agent/main.go, Fiber on :8081, route POST /v1/investigations.

Handler: validate tenant header, parse investigation_id, load envelope from DB (or receive inline envelope for local proof), build Scope via workeradapter.ResolveScopeDefaults, create Executor via investigate.NewExecutor with PGReaders (real RLS), create Loop via orchestrate.NewLoop with injected ModelClient (Mock or Groq selected by MODEL_PROVIDER env), call Run, return InvestigationOutput JSON (Outcome, Report, EscalationReason, AttemptLog). No claim status mutation.

Shared ModelClient seam: internal/investigate/orchestrate/model.go unchanged.

### ModelClient implementations

MockModelClient in internal/investigate/orchestrate/mock_model.go: scripted []ModelResponse, Complete returns next scripted item, deterministic, no validation bypass.

GroqModelClient in internal/investigate/orchestrate/groq_model.go: OpenAI compatible HTTP POST to https://api.groq.com/openai/v1/chat/completions, model from GROQ_MODEL env (default llama-3.1-8b-instant), key from GROQ_API_KEY env, prompt via RenderPrompt, response payload extraction, retry 3x transient with backoff, no SDK import, httpx.

Orchestrator never imports Groq types. Selection via config.ModelProvider.

### Worker to workflow wiring

Worker processor after verify and invest.Build: when exception exists, call WorkflowProvider.StartExecution with claim/investigation IDs. Worker does not block on workflow; workflow calls agent, agent runs loop. For local pull loop qualification without GCW, worker may directly invoke agent via HTTP as fallback when WORKFLOWS_EMULATOR_HOST empty.

Outbox already exists; no new outbox for workflow. Workflow execution ID stored in investigations table for correlation.

### HITL callback

Workflow awaits callback via events.await_callback. Test harness simulates human by POST /callbacks/{callbackId} via GCW REST with decision payload {action: "approve"|"reject", reason, actor}. Workflow resumes, calls API decision endpoint, API validates tenant and authorization, applies claims.Transition to approved or rejected, writes audit event. Agent never mutates.

### Failure matrix wiring

Infrastructure tests use existing gated packages plus new integration tests under tests/integration/phase3_*.go with *_EMULATOR_HOST skips. Cognitive tests reuse orchestrate_test patterns but with real PGReaders and real Loop (only ModelClient mocked).

### Observability

X-Request-ID propagated from API through outbox event attrs, PubSub attributes, worker, workflow argument, agent request, model request, tool request. Logs emit IDs, hashes, counts only. Metrics: existing plus workflow start and callback counters.

## Build plan

Order respects tracer bullet (thin E2E first, then thicken).

1. GCW topology: create claimops-net, run GCW emulator, verify REST list workflows, document docker run commands, add infra/gcw/ README, no compose file.
2. WorkflowProvider interface and GCWProvider REST client with httpx, timeout, unit tests with respx, no emulator required.
3. Workflow definition YAML and deploy script, verify hot reload, test execution hello world through GCW.
4. Agent service: cmd/agent scaffold, POST /v1/investigations handler, wire Loop with MockModelClient, add Dockerfile.agent, healthz, metrics.
5. Wire worker to WorkflowProvider on exception path, store investigation and execution correlation, fallback direct agent call when no GCW.
6. MockModelClient E2E: synthetic claim via API, GCS blob, outbox, PubSub, worker, parser, extract, assemble, verify, exception, workflow, agent, CALL_TOOL real tool, SUBMIT_REPORT grounded, assert REPORT_READY and every boundary persisted.
7. ESCALATED path: synthetic fraud or budget case, assert ESCALATED via deterministic control, no hardcoded outcome.
8. HITL callback: deploy workflow with await_callback, run to wait, simulate human POST callback, verify workflow resume and authoritative Go mutation with tenant check, agent did not mutate.
9. Infrastructure failure matrix: duplicate, restart, localgcp restart, corrupt GCS, PubSub down, Mockoon down, each as gated integration test.
10. Cognitive failure matrix: malformed, undeclared, fabricated, cross tenant, repetition, deadline, budgets, each via real orchestrator and MockModelClient.
11. GroqModelClient: implement OpenAI compatible client, env config, secret handling, no logging of prompt or key.
12. Real model qualification: run frozen eval harness twice (Fake vs Groq), compare, assert hard gates, do not mutate eval.

Each slice is one atomic branch and PR. No slice modifies frozen eval.

## Consequences

Positive: proves final distributed responsibilities, qualifies GCW durability locally, keeps ModelClient as sole cognitive seam, preserves frozen contracts, enables Groq swap with zero orchestration change.

Negative: adds GCW emulator container to local dev, adds WorkflowProvider abstraction, adds agent service deployment surface, requires GROQ_API_KEY for final qualification.

Follow up: Terraform for Workflows and Cloud Run Agent, production IAM for Workflows to Agent, secret manager for Groq key.

## Rationale pointer

See rationale.md for options detail and references.

