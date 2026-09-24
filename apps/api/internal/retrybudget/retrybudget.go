// Package retrybudget pins the cross-layer retry composition bound for
// one logical claim investigation (APA-27/F9 slice).
//
// Each retry layer is individually bounded, but only comments claim the
// composition does not multiply: worker Tier-1 redelivery
// (worker.MaxAttempts per Handle, new-pipeline single attempt, 60s
// wall-clock cap), workflow HTTP steps (max_retries in
// workflows/claim-investigation.yaml), and provider fallback
// (FallbackModelClient MaxTotalCalls default). This package declares the
// finite global bound so widening any single layer budget independently
// turns the composition test red.
package retrybudget

import "claimops-api/internal/worker"

// Layer budgets. WorkflowHTTPMaxRetries mirrors
// workflows/claim-investigation.yaml (every http.post step); it is pinned
// against the YAML at test time, and structurally by
// tests/unit/test_workflow_timeout.py::test_all_posts_bounded.
const (
	// WorkerTier1Attempts bounds Tier-1 pipeline tries per delivery.
	WorkerTier1Attempts = worker.MaxAttempts
	// WorkflowHTTPMaxRetries bounds retries per workflow HTTP step.
	WorkflowHTTPMaxRetries = 3
	// WorkflowHTTPAttempts bounds total attempts per workflow HTTP step.
	WorkflowHTTPAttempts = WorkflowHTTPMaxRetries + 1
	// ProviderMaxTotalCalls bounds provider invocations per model call.
	ProviderMaxTotalCalls = 4
	// WorkerExecutionsPerInvestigation bounds workflow executions per
	// logical investigation across worker (re)deliveries. It is 1, not
	// worker.MaxAttempts: the S6 durable launch boundary converges
	// redeliveries onto one execution instead of minting duplicates.
	WorkerExecutionsPerInvestigation = 1
)

// MaxCompositionProviderCalls is the declared finite global bound: worst
// case, one logical investigation attributes at most one workflow
// execution x workflowHTTPAttempts step attempts x providerCalls
// downstream provider invocations under the single-step model (one
// workflow step per execution, one model call per step attempt).
//
// Derivation: 1 workflow execution per investigation (S6 dedupe, not 3:
// worker redeliveries converge, they never multiply executions) x 4
// workflow HTTP attempts (1 + max_retries 3 per http.post step) x 4
// provider calls (FallbackModelClient MaxTotalCalls default) = 16.
//
// The worker factor collapses 3 -> 1 by mechanism, not by assertion:
//   - Stable identity: worker/launch.go investigationIDForDocument
//     derives the investigation ID deterministically from
//     (tenant, claim, document), so redelivery of the same document
//     yields the same ID instead of minting a fresh one.
//   - Persist-first: investigate/launch.go EnsureLaunched saves the
//     envelope BEFORE any launch attempt, so a crash between persist
//     and start is recoverable by redelivery (never orphan, never skip).
//   - First-wins converge: investigate/launch.go RecordLaunch is
//     idempotent (ON CONFLICT DO NOTHING) and EnsureLaunched looks up
//     launch state first — an already-launched investigation returns
//     the same execution name with zero new StartExecution calls.
//   - No internal retry: investigate/launch.go performs no retries
//     itself; the caller (worker/transport) owns redelivery, so the
//     worker budget never multiplies the workflow budget.
//
// The loop x provider product stays additive-free by a second
// mechanism owned elsewhere (orchestrate/fallback.go markExhausted
// makes a spent fallback chain terminal so the orchestrate loop does
// not re-retry it).
//
// GREEN: layers already compose within this bound, so the bound
// declaration plus the pinning test is the complete change; no retry
// semantics were altered. The prior 48 (3 x 4 x 4) is NOT reachable:
// reaching it would require three distinct workflow executions for one
// investigation, which the boundary above forbids.
//
// Single-step scope note: a workflow execution with N sequential http.post
// steps multiplies the HTTP factor by N; this pin covers the per-step
// composition the F9 gap names, not multi-step fan-out.
const MaxCompositionProviderCalls = WorkerExecutionsPerInvestigation * WorkflowHTTPAttempts * ProviderMaxTotalCalls
