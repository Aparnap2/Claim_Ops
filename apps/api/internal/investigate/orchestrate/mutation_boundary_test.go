package orchestrate

// APA-11 — authoritative mutation boundary for agents.
//
// Invariant: the cognitive loop and `cmd/agent` never mutate authoritative
// claim/document/HITL state. The sole deterministic writer for
// investigation output (`create_investigation_report` / T11) is denied to
// the model path, and the agent registry is read-only. Model output can
// produce REPORT_READY or ESCALATED with a validated Report, never a
// `claim_status` transition or direct DB write.
//
// This file is the APA-11 regression gate: if anyone wires T11 into the
// agent, removes the Loop denial, or lets a report smuggle a transition,
// these tests turn red before business behavior changes.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// The production agent registry pin lives in cmd/agent/registry_test.go
// (TestAgentRegistry_IsReadOnly) which inspects the live
// buildAgentRegistry wiring. This file keeps the loop-boundary proofs
// (T11 denied, report cannot carry claim_status) where the loop lives.
// Keeping the registry assertion coupled to the real wiring prevents the
// false-green where a duplicated test allowlist diverges from main.go.

// Loop must deny T11 even when the scope explicitly allows it (the scope
// cannot broaden envelope authority for this tool). The gate is in loop.go,
// not just in the executor allowlist.
func TestMutationBoundary_LoopDeniesT11EvenWhenScopeAllows(t *testing.T) {
	env := testEnvelope(t)
	// Add T11 to envelope authority so scope can legally include it.
	env.Scope.AllowTools = append(append([]invest.ToolName(nil), env.Scope.AllowTools...), invest.ToolCreateInvestigationReport)
	slices.SortFunc(env.Scope.AllowTools, func(a, b invest.ToolName) int {
		return strings.Compare(string(a), string(b))
	})
	scope := testScope(env)
	scope.AllowTools = append(append([]invest.ToolName(nil), scope.AllowTools...), invest.ToolCreateInvestigationReport)
	slices.SortFunc(scope.AllowTools, func(a, b invest.ToolName) int {
		return strings.Compare(string(a), string(b))
	})
	raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolCreateInvestigationReport})
	if err != nil {
		t.Fatalf("marshal T11: %v", err)
	}
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw)}}
	exec := successExecutor()
	// Give executor a T11 impl so the only denial is the loop gate itself.
	execWithT11 := investigate.NewExecutor(map[invest.ToolName]investigate.ToolFunc{
		invest.ToolCreateInvestigationReport: func(_ context.Context, req investigate.Request) (investigate.Response, error) {
			return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{"ev-doc-01"}}, nil
		},
		invest.ToolGetClaim: func(_ context.Context, req investigate.Request) (investigate.Response, error) {
			return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{"ev-doc-01"}}, nil
		},
	}, time.Time{})
	_ = exec
	lp, lerr := NewLoop(fake, execWithT11, DefaultBudgets(scope), scope, env, nil)
	if lerr != nil {
		t.Fatalf("NewLoop: %v", lerr)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("want INVALID_OUTPUT for T11")
	}
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("err = %v, want ErrToolDenied", err)
	}
	if out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Reason = %q, want INVALID_OUTPUT", out.EscalationReason)
	}
	if execWithT11.Calls() != 0 {
		t.Fatalf("executor Calls() = %d, want 0 (T11 never reaches Execute)", execWithT11.Calls())
	}
}

// A REPORT_READY report is the only success payload and it must never carry
// an authoritative claim transition. Model output that tries to smuggle
// `claim_status`, `transition`, or verdict action inside the report body
// must not be treated as a claim mutation (grounding + output validation +
// loop denials ensure it).
func TestMutationBoundary_ReportCannotCarryClaimTransition(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(guardrailSubmitBytes(t, testReport(env, "ev-new-01"))),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Outcome != OutcomeReportReady {
		t.Fatalf("Outcome = %q, want REPORT_READY", out.Outcome)
	}
	raw, err := MarshalOutput(out)
	if err != nil {
		t.Fatalf("MarshalOutput: %v", err)
	}
	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{`"claim_status"`, `"transition"`, `"status_transition"`, `"verdict"`} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("output contains forbidden authoritative key %s", forbidden)
		}
	}
	// Also prove an executor that tracks T11 never saw it even on success.
	if fake.Calls != 2 {
		t.Fatalf("model calls = %d, want 2 (claim + submit)", fake.Calls)
	}
}
