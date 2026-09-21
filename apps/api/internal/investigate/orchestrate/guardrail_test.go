package orchestrate

// Gate 1 — file 3 of 4: guardrail / authority fencing (APA-21).
//
// Deterministic, no Groq. Reuses FakeModelClient / MockModelClient,
// testEnvelope, testScope and the invest/investigate fixtures from
// orchestrate_test.go. Tests actual production seams (Loop, Executor,
// Scope, ModelClient, grounding): model output never becomes authoritative
// state directly, execution budgets and authority checks fail closed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// ---------------------------------------------------------------------------
// Helpers local to guardrail tests (no collision with orchestrate_test.go)
// ---------------------------------------------------------------------------

func guardrailCallToolBytes(t *testing.T, env invest.UnresolvedException, scope investigate.Scope, tool invest.ToolName, limit int) []byte {
	t.Helper()
	return callToolBytes(t, env, scope, tool, limit)
}

func guardrailSubmitBytes(t *testing.T, r Report) []byte {
	t.Helper()
	return submitBytes(t, r)
}

// guardrailExecutor counts backend invocations per tool name.
type guardrailCountingExecutor struct {
	calls map[invest.ToolName]int
	exec  *investigate.Executor
}

func newGuardrailCountingExecutor(t *testing.T, scope investigate.Scope, ids map[invest.ToolName]string) (*investigate.Executor, *guardrailCountingExecutor) {
	t.Helper()
	tracker := &guardrailCountingExecutor{calls: map[invest.ToolName]int{}}
	tools := map[invest.ToolName]investigate.ToolFunc{}
	for tool, id := range ids {
		toolCopy := tool
		idCopy := id
		tools[toolCopy] = func(_ context.Context, req investigate.Request) (investigate.Response, error) {
			tracker.calls[req.Tool]++
			return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{idCopy}}, nil
		}
	}
	// Ensure every allowlisted scope tool has an impl so executor don't reject as missing impl before we test.
	for _, tool := range scope.AllowTools {
		if _, ok := tools[tool]; !ok {
			toolCopy := tool
			tools[toolCopy] = func(_ context.Context, req investigate.Request) (investigate.Response, error) {
				tracker.calls[req.Tool]++
				return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{"ev-doc-01"}}, nil
			}
		}
	}
	exec := investigate.NewExecutor(tools, time.Time{})
	tracker.exec = exec
	return exec, tracker
}

// ---------------------------------------------------------------------------
// 1. Input guardrails — fail closed before any tool call
// ---------------------------------------------------------------------------

func TestGuardrail_Input_MalformedEnvelope(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)

	t.Run("blank tenant envelope", func(t *testing.T) {
		bad := env
		bad.TenantID = "   "
		fake := &FakeModelClient{}
		_, err := NewLoop(fake, successExecutor(), DefaultBudgets(scope), scope, bad, nil)
		if err == nil {
			t.Fatal("want construction error for blank tenant")
		}
		if !errors.Is(err, ErrModelContract) && !strings.Contains(err.Error(), "contract") && !strings.Contains(err.Error(), "tenant") {
			t.Fatalf("err = %v, want contract-class error", err)
		}
		if fake.Calls != 0 {
			t.Fatalf("model calls = %d, want 0 before tool", fake.Calls)
		}
	})

	t.Run("bad investigation id", func(t *testing.T) {
		bad := env
		bad.InvestigationID = "inv-bad"
		fake := &FakeModelClient{}
		_, err := NewLoop(fake, successExecutor(), DefaultBudgets(scope), scope, bad, nil)
		if err == nil {
			t.Fatal("want construction error for bad investigation id")
		}
		if !errors.Is(err, ErrModelContract) {
			// invest.Validate wraps contract; accept any contract-class error.
			if !strings.Contains(err.Error(), "contract") && !strings.Contains(err.Error(), "id") {
				t.Fatalf("err = %v, want contract class", err)
			}
		}
		if fake.Calls != 0 {
			t.Fatalf("model calls = %d, want 0", fake.Calls)
		}
	})

	t.Run("empty envelope invalid exception", func(t *testing.T) {
		bad := env
		bad.RuleFindings = nil
		bad.Unresolved = nil
		fake := &FakeModelClient{}
		_, err := NewLoop(fake, successExecutor(), DefaultBudgets(scope), scope, bad, nil)
		if err == nil {
			t.Fatal("want construction error for empty envelope")
		}
		if fake.Calls != 0 {
			t.Fatalf("model calls = %d, want 0", fake.Calls)
		}
	})
}

func TestGuardrail_Input_OversizedInput(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	b := DefaultBudgets(scope)
	b.MaxInputBytes = 1
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
	}}
	lp, err := NewLoop(fake, successExecutor(), b, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	exec := successExecutor()
	// Replace lp exec with counting to prove no tool executed (lt: NewLoop already validated, but loop uses its own exec).
	// We already passed exec via NewLoop; now verify via that exec's Calls().
	_ = exec
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("want INVALID_OUTPUT for oversized canonical request")
	}
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want ErrModelContract", err)
	}
	if invalidKindOf(err) != invalidOversize {
		t.Fatalf("kind = %s, want I8-oversize", invalidKindString(invalidKindOf(err)))
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
	}
	if fake.Calls != 0 {
		t.Fatalf("model calls = %d, want 0 (cap checked before Complete)", fake.Calls)
	}
}

func TestGuardrail_Input_ScopeMismatch(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)

	t.Run("tenant mismatch F6", func(t *testing.T) {
		mismatched := scope
		mismatched.TenantID = "tnt-other"
		b := DefaultBudgets(mismatched)
		fake := &FakeModelClient{}
		_, err := NewLoop(fake, successExecutor(), b, mismatched, env, nil)
		if err == nil {
			t.Fatal("want F6 tenant mismatch")
		}
		if !errors.Is(err, ErrTenantMismatch) {
			t.Fatalf("err = %v, want ErrTenantMismatch", err)
		}
		if fake.Calls != 0 {
			t.Fatalf("model calls = %d, want 0", fake.Calls)
		}
		// Also prove mid-run still fails closed if mutated after construction (F6 dynamic split).
		fake2 := &FakeModelClient{Responses: []ModelResponse{
			modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		}}
		lp := newTestLoop(t, fake2, successExecutor(), scope, env)
		lp.scope.TenantID = "tnt-other"
		out, err := lp.Run(context.Background())
		if !errors.Is(err, ErrTenantMismatch) {
			t.Fatalf("mid-run err = %v, want ErrTenantMismatch", err)
		}
		if out.Outcome != "" {
			t.Fatalf("Outcome = %q, want empty on F6 abort", out.Outcome)
		}
		if fake2.Calls != 0 {
			t.Fatalf("model calls = %d, want 0 before F6 abort", fake2.Calls)
		}
	})

	t.Run("claim mismatch", func(t *testing.T) {
		mismatched := scope
		mismatched.ClaimID = "clm-other"
		b := DefaultBudgets(mismatched)
		fake := &FakeModelClient{}
		_, err := NewLoop(fake, successExecutor(), b, mismatched, env, nil)
		if err == nil {
			t.Fatal("want claim mismatch contract error")
		}
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		if fake.Calls != 0 {
			t.Fatalf("model calls = %d, want 0", fake.Calls)
		}
	})

	t.Run("scope tools outside envelope authority", func(t *testing.T) {
		mismatched := scope
		mismatched.AllowTools = append(append([]invest.ToolName(nil), scope.AllowTools...), invest.ToolGetTPACase)
		b := DefaultBudgets(mismatched)
		fake := &FakeModelClient{}
		_, err := NewLoop(fake, successExecutor(), b, mismatched, env, nil)
		if err == nil {
			t.Fatal("want tool outside envelope authority")
		}
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		if !strings.Contains(err.Error(), "outside envelope authority") {
			t.Fatalf("err = %v, want outside envelope authority message", err)
		}
		if fake.Calls != 0 {
			t.Fatalf("model calls = %d, want 0", fake.Calls)
		}
	})

	t.Run("scope max_calls exceeds envelope", func(t *testing.T) {
		mismatched := scope
		mismatched.MaxCalls = env.Scope.MaxToolCalls + 1
		b := Budgets{
			MaxTurns:       DefaultMaxTurns,
			MaxModelCalls:  DefaultMaxModelCalls,
			MaxToolCalls:   mismatched.MaxCalls,
			MaxInputBytes:  DefaultMaxInputBytes,
			MaxOutputBytes: DefaultMaxOutputBytes,
			TurnTimeoutMs:  DefaultTurnTimeoutMs,
		}
		fake := &FakeModelClient{}
		_, err := NewLoop(fake, successExecutor(), b, mismatched, env, nil)
		if err == nil {
			t.Fatal("want max_calls exceeds envelope")
		}
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
	})

	t.Run("scope request mismatch", func(t *testing.T) {
		mismatched := scope
		mismatched.RequestID = "req-other"
		b := DefaultBudgets(mismatched)
		fake := &FakeModelClient{}
		_, err := NewLoop(fake, successExecutor(), b, mismatched, env, nil)
		if err == nil {
			t.Fatal("want request mismatch")
		}
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
	})

	t.Run("scope deadline outlives envelope", func(t *testing.T) {
		mismatched := scope
		mismatched.DeadlineMs = env.Scope.DeadlineMs + 1000
		b := DefaultBudgets(mismatched)
		// Need to bypass ValidateBudgets max deadline cap; envelope deadline is within cap so we just need to keep scope deadline <= MaxDeadlineMs.
		fake := &FakeModelClient{}
		_, err := NewLoop(fake, successExecutor(), b, mismatched, env, nil)
		if err == nil {
			t.Fatal("want deadline outlives envelope")
		}
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
	})
}

// ---------------------------------------------------------------------------
// 2. Model-output guardrails — injected via scripted FakeModelClient
// ---------------------------------------------------------------------------

func TestGuardrail_ModelOutput_UndeclaredTool(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	// Fabricate a tool name not in the 11-tool allowlist.
	payload, err := json.Marshal(ModelAction{
		Action: ActionCallTool,
		Tool:   invest.ToolName("evil_tool"),
		Request: func() *investigate.Request {
			r, _ := investigate.NewRequest(invest.ToolGetClaim, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, 1)
			return &r
		}(),
	})
	if err != nil {
		t.Fatalf("marshal evil payload: %v", err)
	}
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(payload)}}
	exec := successExecutor()
	out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("want INVALID_OUTPUT for undeclared tool")
	}
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("err = %v, want ErrToolDenied", err)
	}
	if invalidKindOf(err) != invalidDenied {
		t.Fatalf("kind = %s, want I3-denied", invalidKindString(invalidKindOf(err)))
	}
	if out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Reason = %q, want INVALID_OUTPUT", out.EscalationReason)
	}
	if exec.Calls() != 0 {
		t.Fatalf("executor Calls() = %d, want 0 (denied before Execute)", exec.Calls())
	}
}

func TestGuardrail_ModelOutput_FabricatedEvidenceID(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	rep := testReport(env)
	rep.Hypotheses[0].EvidenceIDs = []string{"ev-doc-01", "ev-fabricated-99"}
	rep.Findings[0].EvidenceIDs = []string{"ev-doc-01", "ev-fabricated-99"}
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(guardrailSubmitBytes(t, rep))}}
	exec := successExecutor()
	out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("want grounding rejection")
	}
	if !errors.Is(err, ErrGrounding) {
		t.Fatalf("err = %v, want ErrGrounding", err)
	}
	if invalidKindOf(err) != invalidGrounding {
		t.Fatalf("kind = %s, want I6-grounding", invalidKindString(invalidKindOf(err)))
	}
	if out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Reason = %q, want INVALID_OUTPUT", out.EscalationReason)
	}
	if out.Partial == nil {
		t.Fatal("grounding rejection of structural report should carry Partial")
	}
	if exec.Calls() != 0 {
		t.Fatalf("executor Calls() = %d, want 0 (grounding fails before tool)", exec.Calls())
	}
}

func TestGuardrail_ModelOutput_HallucinatedRequestIDs(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	cases := []struct {
		name string
		mut  func(investigate.Request) investigate.Request
	}{
		{"tenant", func(r investigate.Request) investigate.Request { r.TenantID = "tnt-other"; return r }},
		{"claim", func(r investigate.Request) investigate.Request { r.ClaimID = "clm-other"; return r }},
		{"request", func(r investigate.Request) investigate.Request { r.RequestID = "req-other"; return r }},
		{"investigation", func(r investigate.Request) investigate.Request {
			r.InvestigationID = "inv-00000000000000000000000000000000"
			return r
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := investigate.NewRequest(invest.ToolGetClaim, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, 1)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			bad := tc.mut(req)
			raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolGetClaim, Request: &bad})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw)}}
			exec := successExecutor()
			out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
			if err == nil {
				t.Fatal("want INVALID_OUTPUT for hallucinated request id")
			}
			if !errors.Is(err, ErrModelContract) {
				t.Fatalf("err = %v, want ErrModelContract", err)
			}
			if invalidKindOf(err) != invalidRequest {
				t.Fatalf("kind = %s, want I4-request", invalidKindString(invalidKindOf(err)))
			}
			if out.EscalationReason != EscalationInvalidOutput {
				t.Fatalf("Reason = %q, want INVALID_OUTPUT", out.EscalationReason)
			}
			if exec.Calls() != 0 {
				t.Fatalf("executor Calls() = %d, want 0 (rejected before Execute)", exec.Calls())
			}
		})
	}
}

func TestGuardrail_ModelOutput_UnknownFields(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	validCall := guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)
	withUnknown := withUnknownField(t, validCall, `"hallucinated_field":"oops"`)
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(withUnknown), modelResp(withUnknown)}}
	exec := successExecutor()
	out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("want INVALID_OUTPUT for unknown fields")
	}
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want ErrModelContract", err)
	}
	if invalidKindOf(err) != invalidMalformed {
		t.Fatalf("kind = %s, want I1-malformed", invalidKindString(invalidKindOf(err)))
	}
	if out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Reason = %q, want INVALID_OUTPUT", out.EscalationReason)
	}
	if exec.Calls() != 0 {
		t.Fatalf("executor Calls() = %d, want 0", exec.Calls())
	}
	if fake.Calls != 2 {
		t.Fatalf("model calls = %d, want 2 (one reprompt then escalate)", fake.Calls)
	}
}

func TestGuardrail_ModelOutput_OversizedPayload(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	b := DefaultBudgets(scope)
	b.MaxOutputBytes = 10
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
	}}
	lp, err := NewLoop(fake, successExecutor(), b, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("want oversize escalation")
	}
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want ErrModelContract", err)
	}
	if invalidKindOf(err) != invalidOversize {
		t.Fatalf("kind = %s, want I8-oversize", invalidKindString(invalidKindOf(err)))
	}
	if out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Reason = %q, want INVALID_OUTPUT", out.EscalationReason)
	}
	// Oversize is checked before decode; never consumes tool budget.
	if fake.Calls != 1 {
		t.Fatalf("model calls = %d, want 1 (no reprompt for oversize)", fake.Calls)
	}
}

func TestGuardrail_ModelOutput_EmptyPayloadReprompt(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	// Empty payload -> I7, reprompt once, second empty -> escalate.
	fake := &FakeModelClient{Responses: []ModelResponse{
		{Payload: []byte("   "), ModelID: "fake-model-01"},
		{Payload: []byte(""), ModelID: "fake-model-01"},
	}}
	exec := successExecutor()
	out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("want empty escalation")
	}
	if !errors.Is(err, ErrModelEmpty) {
		t.Fatalf("err = %v, want ErrModelEmpty", err)
	}
	if invalidKindOf(err) != invalidEmpty {
		t.Fatalf("kind = %s, want I7-empty", invalidKindString(invalidKindOf(err)))
	}
	if out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Reason = %q, want INVALID_OUTPUT", out.EscalationReason)
	}
	if fake.Calls != 2 {
		t.Fatalf("model calls = %d, want 2 (empty reprompt)", fake.Calls)
	}
	if exec.Calls() != 0 {
		t.Fatalf("executor Calls() = %d, want 0", exec.Calls())
	}
	if len(out.AttemptLog) != 0 {
		t.Fatalf("AttemptLog = %v, want empty (no tool executed)", out.AttemptLog)
	}
}

func TestGuardrail_ModelOutput_EmptyThenRepair(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		{Payload: []byte(""), ModelID: "fake-model-01"},
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(guardrailSubmitBytes(t, testReport(env, "ev-new-01"))),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err != nil {
		t.Fatalf("Run after empty repair: %v", err)
	}
	if out.Outcome != OutcomeReportReady {
		t.Fatalf("Outcome = %q, want REPORT_READY after one empty reprompt", out.Outcome)
	}
	if fake.Calls != 3 {
		t.Fatalf("model calls = %d, want 3 (empty + repair + submit)", fake.Calls)
	}
}

// ---------------------------------------------------------------------------
// 3. Execution guardrails — budgets, repetition, no-progress, deadline
// ---------------------------------------------------------------------------

func TestGuardrail_Execution_MaxTurns(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	b := DefaultBudgets(scope)
	b.MaxTurns = 1
	b.MaxModelCalls = 1
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(guardrailSubmitBytes(t, testReport(env, "ev-new-01"))),
	}}
	exec := successExecutor()
	lp, err := NewLoop(fake, exec, b, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("want TURNS_EXHAUSTED")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if out.EscalationReason != EscalationTurnsExhausted {
		t.Fatalf("Reason = %q, want TURNS_EXHAUSTED", out.EscalationReason)
	}
	if exec.Calls() != 1 {
		t.Fatalf("executor Calls() = %d, want 1 (only first turn executed)", exec.Calls())
	}
	if fake.Calls != 1 {
		t.Fatalf("model calls = %d, want 1 (second turn never started)", fake.Calls)
	}
	if out.ToolCallsUsed != 1 || out.TurnsUsed != 1 {
		t.Fatalf("TurnsUsed=%d ToolCallsUsed=%d, want 1/1", out.TurnsUsed, out.ToolCallsUsed)
	}
}

func TestGuardrail_Execution_MaxModelCalls(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	// Tool-call budget is the reachable CALLS_EXHAUSTED path: model-call budget
	// must be >= MaxTurns by ValidateBudgets, so model-call exhaustion is only
	// observable via reprompt overhead. Here we prove tool-call exhaustion
	// (also CALLS_EXHAUSTED) via scope.MaxCalls.
	scope.MaxCalls = 2
	b := DefaultBudgets(scope)
	b.MaxTurns = 5
	b.MaxModelCalls = 5
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetEvidence, 1)),
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetEvidence, 2)),
	}}
	exec, _ := newGuardrailCountingExecutor(t, scope, map[invest.ToolName]string{
		invest.ToolGetClaim:    "ev-new-01",
		invest.ToolGetEvidence: "ev-new-02",
	})
	lp, err := NewLoop(fake, exec, b, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("want CALLS_EXHAUSTED")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if out.EscalationReason != EscalationCallsExhausted {
		t.Fatalf("Reason = %q, want CALLS_EXHAUSTED", out.EscalationReason)
	}
	if fake.Calls != 3 {
		t.Fatalf("model calls = %d, want 3 (3rd attempted before tool budget)", fake.Calls)
	}
	if exec.Calls() != 2 {
		t.Fatalf("executor Calls() = %d, want 2 (no third tool beyond model budget)", exec.Calls())
	}
}

func TestGuardrail_Execution_MaxToolCalls(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	scope.MaxCalls = 1
	b := DefaultBudgets(scope)
	// Second tool call should exceed budget.
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetEvidence, 1)),
	}}
	exec, _ := newGuardrailCountingExecutor(t, scope, map[invest.ToolName]string{
		invest.ToolGetClaim:    "ev-new-01",
		invest.ToolGetEvidence: "ev-new-02",
	})
	lp, err := NewLoop(fake, exec, b, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("want CALLS_EXHAUSTED for tool budget")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if out.EscalationReason != EscalationCallsExhausted {
		t.Fatalf("Reason = %q, want CALLS_EXHAUSTED", out.EscalationReason)
	}
	if exec.Calls() != 1 {
		t.Fatalf("executor Calls() = %d, want 1 (second tool rejected before Execute)", exec.Calls())
	}
	if out.ToolCallsUsed != 1 {
		t.Fatalf("ToolCallsUsed = %d, want 1", out.ToolCallsUsed)
	}
}

func TestGuardrail_Execution_Repetition(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	call := guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(call), modelResp(call)}}
	exec, tracker := newGuardrailCountingExecutor(t, scope, map[invest.ToolName]string{
		invest.ToolGetClaim: "ev-new-01",
	})
	lp, err := NewLoop(fake, exec, DefaultBudgets(scope), scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("want REPETITION")
	}
	if !errors.Is(err, ErrRepetition) {
		t.Fatalf("err = %v, want ErrRepetition", err)
	}
	if out.EscalationReason != EscalationRepetition {
		t.Fatalf("Reason = %q, want REPETITION", out.EscalationReason)
	}
	if exec.Calls() != 1 {
		t.Fatalf("executor Calls() = %d, want 1 (repeat rejected before second Execute)", exec.Calls())
	}
	if tracker.calls[invest.ToolGetClaim] != 1 {
		t.Fatalf("tool-specific count = %d, want 1", tracker.calls[invest.ToolGetClaim])
	}
	if out.ToolCallsUsed != 1 {
		t.Fatalf("ToolCallsUsed = %d, want 1", out.ToolCallsUsed)
	}
	if fake.Calls != 2 {
		t.Fatalf("model calls = %d, want 2 (first + repeat turn)", fake.Calls)
	}
}

func TestGuardrail_Execution_NoProgress(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	// Three stagnant turns: upstream failures widen nothing and differ in Limit to avoid repetition.
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetEvidence, 1)),
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetEvidence, 2)),
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetEvidence, 3)),
	}}
	exec := failingExecutor()
	out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("want NO_PROGRESS")
	}
	if out.EscalationReason != EscalationNoProgress {
		t.Fatalf("Reason = %q, want NO_PROGRESS", out.EscalationReason)
	}
	if !errors.Is(err, investigate.ErrUpstream) {
		t.Fatalf("err = %v, want cause chain to carry investigate.ErrUpstream", err)
	}
	if len(out.AttemptLog) != 3 {
		t.Fatalf("AttemptLog = %d, want 3 stagnant", len(out.AttemptLog))
	}
	if exec.Calls() != 3 {
		t.Fatalf("executor Calls() = %d, want 3 (one per stagnant turn, no extra beyond threshold)", exec.Calls())
	}
	// Fourth turn must not have been executed: model calls stops at 3.
	if fake.Calls != 3 {
		t.Fatalf("model calls = %d, want 3", fake.Calls)
	}
}

func TestGuardrail_Execution_Deadline(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	scope.DeadlineMs = 100
	fake := &FakeModelClient{
		Delay: 200 * time.Millisecond,
		Responses: []ModelResponse{
			modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		},
	}
	exec, _ := newGuardrailCountingExecutor(t, scope, map[invest.ToolName]string{
		invest.ToolGetClaim: "ev-new-01",
	})
	lp, err := NewLoop(fake, exec, DefaultBudgets(scope), scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("want DEADLINE")
	}
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want ErrDeadlineExceeded", err)
	}
	if out.EscalationReason != EscalationDeadline {
		t.Fatalf("Reason = %q, want DEADLINE", out.EscalationReason)
	}
	if exec.Calls() != 0 {
		t.Fatalf("executor Calls() = %d, want 0 (deadline before Execute)", exec.Calls())
	}
}

// ---------------------------------------------------------------------------
// 4. State / authority guardrails — model output never becomes authoritative state
// ---------------------------------------------------------------------------

func TestGuardrail_State_NeverCallsT11(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	// Even when model explicitly asks for T11, loop must reject without executing any tool.
	raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolCreateInvestigationReport})
	if err != nil {
		t.Fatalf("marshal t11: %v", err)
	}
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw)}}
	exec := successExecutor()
	out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("want INVALID_OUTPUT for T11")
	}
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("err = %v, want ErrToolDenied", err)
	}
	if invalidKindOf(err) != invalidDenied {
		t.Fatalf("kind = %s, want I3-denied", invalidKindString(invalidKindOf(err)))
	}
	if out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Reason = %q, want INVALID_OUTPUT", out.EscalationReason)
	}
	if exec.Calls() != 0 {
		t.Fatalf("executor Calls() = %d, want 0 (T11 never callable)", exec.Calls())
	}
	if fake.Calls != 1 {
		t.Fatalf("model calls = %d, want 1 (never reprompted for denied)", fake.Calls)
	}
	// Also assert via MockModelClient seam (second client reuse required by constraints).
	mock := NewMockModelClient([]ModelResponse{modelResp(raw)})
	out2, err2 := newTestLoop(t, mock, successExecutor(), scope, env).Run(context.Background())
	if !errors.Is(err2, ErrToolDenied) {
		t.Fatalf("mock err = %v, want ErrToolDenied", err2)
	}
	if out2.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("mock Reason = %q, want INVALID_OUTPUT", out2.EscalationReason)
	}
}

func TestGuardrail_State_ReportIsOnlyValidatedPath(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(guardrailSubmitBytes(t, testReport(env, "ev-new-01"))),
	}}
	exec := successExecutor()
	out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Outcome != OutcomeReportReady {
		t.Fatalf("Outcome = %q, want REPORT_READY", out.Outcome)
	}
	// Validate loop output before any state effect.
	if err := ValidateInvestigationOutput(out); err != nil {
		t.Fatalf("ValidateInvestigationOutput: %v", err)
	}
	// Report is the only payload; marshal does not carry claim status or transition.
	raw, err := MarshalOutput(out)
	if err != nil {
		t.Fatalf("MarshalOutput: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	for _, forbidden := range []string{"claim_status", "transition", "verdict", "status_transition"} {
		if _, ok := m[forbidden]; ok {
			t.Fatalf("output carries forbidden authoritative key %q", forbidden)
		}
		// Also scan report body for those keys.
		if bytes.Contains(bytes.ToLower(raw), []byte(`"`+forbidden+`"`)) {
			t.Fatalf("raw output contains forbidden key %q", forbidden)
		}
	}
	// Inside report, no transition vocabulary either.
	var rep Report
	if out.Report != nil {
		rep = *out.Report
	}
	repRaw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	for _, bad := range []string{"APPROVE", "DENY", "PAY", "REJECT"} {
		if strings.Contains(string(repRaw), `"action":"`+bad+`"`) {
			t.Fatalf("report smuggles verdict %q", bad)
		}
	}
	// Writer never invoked even on success.
	if exec.Calls() != 1 {
		t.Fatalf("executor Calls() = %d, want 1 (no T11)", exec.Calls())
	}
}

func TestGuardrail_State_EscalationCarriesTypedReason(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	// Trigger each escalation class and verify typed reason + audit row counts.
	cases := []struct {
		name   string
		reason EscalationReason
		run    func(t *testing.T) (InvestigationOutput, error)
	}{
		{
			name:   "INVALID_OUTPUT",
			reason: EscalationInvalidOutput,
			run: func(t *testing.T) (InvestigationOutput, error) {
				bad := withUnknownField(t, guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1), `"bogus":1`)
				fake := &FakeModelClient{Responses: []ModelResponse{modelResp(bad), modelResp(bad)}}
				return newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
			},
		},
		{
			name:   "REPETITION",
			reason: EscalationRepetition,
			run: func(t *testing.T) (InvestigationOutput, error) {
				call := guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1)
				fake := &FakeModelClient{Responses: []ModelResponse{modelResp(call), modelResp(call)}}
				return newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
			},
		},
		{
			name:   "CALLS_EXHAUSTED",
			reason: EscalationCallsExhausted,
			run: func(t *testing.T) (InvestigationOutput, error) {
				sc := scope
				sc.MaxCalls = 1
				b := DefaultBudgets(sc)
				fake := &FakeModelClient{Responses: []ModelResponse{
					modelResp(guardrailCallToolBytes(t, env, sc, invest.ToolGetClaim, 1)),
					modelResp(guardrailCallToolBytes(t, env, sc, invest.ToolGetEvidence, 1)),
				}}
				exec, _ := newGuardrailCountingExecutor(t, sc, map[invest.ToolName]string{
					invest.ToolGetClaim: "ev-new-01",
				})
				lp, _ := NewLoop(fake, exec, b, sc, env, nil)
				return lp.Run(context.Background())
			},
		},
		{
			name:   "DEADLINE",
			reason: EscalationDeadline,
			run: func(t *testing.T) (InvestigationOutput, error) {
				sc := scope
				sc.DeadlineMs = 100
				fake := &FakeModelClient{Delay: 200 * time.Millisecond, Responses: []ModelResponse{
					modelResp(guardrailCallToolBytes(t, env, sc, invest.ToolGetClaim, 1)),
				}}
				lp, _ := NewLoop(fake, successExecutor(), DefaultBudgets(sc), sc, env, nil)
				return lp.Run(context.Background())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.run(t)
			if err == nil {
				t.Fatal("want escalation error")
			}
			if out.EscalationReason != tc.reason {
				t.Fatalf("Reason = %q, want %q", out.EscalationReason, tc.reason)
			}
			if out.Outcome != OutcomeEscalated {
				t.Fatalf("Outcome = %q, want ESCALATED", out.Outcome)
			}
			if out.Report != nil {
				t.Fatalf("escalated output must not carry Report, got %v", out.Report)
			}
			if err := ValidateInvestigationOutput(out); err != nil {
				t.Fatalf("ValidateInvestigationOutput: %v", err)
			}
			// No authoritative claim transition in escalated either.
			raw, _ := MarshalOutput(out)
			if bytes.Contains(bytes.ToLower(raw), []byte(`"transition"`)) {
				t.Fatal("escalated output must not carry transition")
			}
		})
	}
}

func TestGuardrail_State_OutputNeverMutatesClaim(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	// Even after REPORT_READY and ESCALATED, loop output never includes a claim status field.
	t.Run("report_ready has no claim mutation", func(t *testing.T) {
		fake := &FakeModelClient{Responses: []ModelResponse{
			modelResp(guardrailSubmitBytes(t, testReport(env))),
		}}
		out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		raw, _ := MarshalOutput(out)
		lower := strings.ToLower(string(raw))
		for _, bad := range []string{"claim_status", "claim status", "approved", "denied"} {
			// Only check key-shaped occurrences; finding text may contain domain words but never as a status transition.
			if strings.Contains(lower, `"`+bad+`"`) {
				t.Fatalf("output contains forbidden authoritative token %q", bad)
			}
		}
	})
	t.Run("escalated has no claim mutation", func(t *testing.T) {
		bad := withUnknownField(t, guardrailCallToolBytes(t, env, scope, invest.ToolGetClaim, 1), `"bogus":1`)
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(bad), modelResp(bad)}}
		out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
		if err == nil {
			t.Fatal("want escalation")
		}
		raw, _ := MarshalOutput(out)
		if strings.Contains(strings.ToLower(string(raw)), `"claim_status"`) {
			t.Fatal("escalated output must not carry claim_status")
		}
	})
}
