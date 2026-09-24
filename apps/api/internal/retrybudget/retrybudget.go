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
)

// MaxCompositionProviderCalls is the declared finite global bound: worst
// case, one logical investigation attributes at most workerAttempts x
// workflowHTTPAttempts x providerCalls downstream invocations under the
// single-step model (one workflow step per worker attempt, one model
// call per step attempt).
//
// Derivation: 3 worker Tier-1 attempts (worker.MaxAttempts) x 4
// workflow HTTP attempts (1 + max_retries 3 per http.post step) x 4
// provider calls (FallbackModelClient MaxTotalCalls default) = 48.
//
// Anti-multiplication mechanisms this bound relies on (owned elsewhere,
// referenced not duplicated): S6 EnsureLaunched converges redeliveries
// onto one investigation instead of minting duplicates (worker x
// workflow), and markExhausted makes a spent fallback chain terminal so
// the orchestrate loop does not re-retry it (loop x provider).
//
// GREEN: layers already compose within this bound, so the bound
// declaration plus the pinning test is the complete change; no retry
// semantics were altered.
//
// Single-step scope note: a workflow execution with N sequential http.post
// steps multiplies the HTTP factor by N; this pin covers the per-step
// composition the F9 gap names, not multi-step fan-out.
const MaxCompositionProviderCalls = WorkerTier1Attempts * WorkflowHTTPAttempts * ProviderMaxTotalCalls
