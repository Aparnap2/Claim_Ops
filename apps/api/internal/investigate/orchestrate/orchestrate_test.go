package orchestrate

// Tests for the bounded investigation orchestrator (issue #66).
//
// All model access goes through the scripted FakeModelClient and all tool
// access through stub investigate.ToolFunc registries: no network, no
// storage, no live infra. The loop is deterministic, so replay tests can
// demand byte-identical output.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"claimops-api/internal/assemble"
	"claimops-api/internal/extract"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/verify"
	"claimops-api/internal/verifywrap"
)

// ---------------------------------------------------------------------------
// FakeModelClient
// ---------------------------------------------------------------------------

// FakeModelClient serves one scripted response/error per Complete call and
// counts calls. Errs[i] != nil fails call i; otherwise Responses[i] is
// returned (a short script repeats its last response so over-long runs
// fail closed in the loop rather than in the fake).
type FakeModelClient struct {
	Responses []ModelResponse
	Errs      []error
	Delay     time.Duration
	Calls     int
}

// Complete implements ModelClient.
func (f *FakeModelClient) Complete(ctx context.Context, _ ModelRequest) (ModelResponse, error) {
	i := f.Calls
	f.Calls++
	if f.Delay > 0 {
		select {
		case <-ctx.Done():
			return ModelResponse{}, ctx.Err()
		case <-time.After(f.Delay):
		}
	}
	if i < len(f.Errs) && f.Errs[i] != nil {
		return ModelResponse{}, f.Errs[i]
	}
	if i < len(f.Responses) {
		return f.Responses[i], nil
	}
	if len(f.Responses) > 0 {
		return f.Responses[len(f.Responses)-1], nil
	}
	return ModelResponse{Payload: []byte(`{}`), ModelID: "fake-model-01"}, nil
}

func modelResp(payload []byte) ModelResponse {
	return ModelResponse{Payload: payload, ModelID: "fake-model-01"}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	tTenant = "tnt-66-orch"
	tClaim  = "clm-66-orch"
	tExID   = "ex-0123456789abcdef0123456789abcdef"
	tInvID  = "inv-abcdef0123456789abcdef0123456789"
	tReqID  = "req-66-orch"
)

// testEnvelope builds a minimal valid invest.UnresolvedException from the
// same origins invest.Build consumes (verify.Result, CanonicalClaim,
// verifywrap.Unresolved, EvidenceRef rows).
func testEnvelope(t *testing.T) invest.UnresolvedException {
	t.Helper()
	evidence := []invest.EvidenceRef{
		{
			EvidenceID: "ev-doc-01", SourceType: invest.EvidenceSourceDocument,
			SourceID: "doc-01", TenantID: tTenant, ClaimID: tClaim,
			DocumentID: "doc-01", Page: 1, BlockID: "b1",
		},
		{
			EvidenceID: "ev-doc-02", SourceType: invest.EvidenceSourceDocument,
			SourceID: "doc-02", TenantID: tTenant, ClaimID: tClaim,
			DocumentID: "doc-02", Page: 1, BlockID: "b2",
		},
		{
			EvidenceID: "ev-pol-01", SourceType: invest.EvidenceSourcePolicy,
			SourceID: "pol-01", ContentHash: "sha256:9f2c4a",
			TenantID: tTenant, ClaimID: tClaim,
		},
	}
	claim := assemble.CanonicalClaim{
		Fields: map[string]assemble.AssembledField{
			"policy_number": {
				Key: "policy_number", Status: assemble.StatusConflict,
			},
			"hospital_name": {
				Key: "hospital_name", Status: assemble.StatusAgreed,
				Agreed: "City Hospital",
				Sources: []assemble.FieldSource{
					{
						Value: "City Hospital", Normalized: "City Hospital",
						Evidence:         extract.EvidenceRef{DocumentID: "doc-02", Page: 1, BlockID: "b2"},
						Extractor:        "liteparse",
						ExtractorVersion: "v3",
						DocType:          "CLAIM_FORM",
					},
				},
			},
		},
		DocsPresent: map[string]bool{
			verify.DocClaimForm:        true,
			verify.DocDischargeSummary: true,
			verify.DocHospitalBill:     true,
		},
		DocTypes: []string{verify.DocClaimForm, verify.DocDischargeSummary, verify.DocHospitalBill},
		DocIDs:   []string{"doc-01", "doc-02"},
	}
	unresolved := []verifywrap.Unresolved{
		{
			Key:    "policy_number",
			Status: assemble.StatusConflict,
			Conflict: &assemble.ConflictEntry{
				Key:      "policy_number",
				Distinct: []string{"POL-X", "POL-Y"},
				Sources: []assemble.FieldSource{
					{
						Value: "POL-X", Normalized: "POL-X",
						Evidence:         extract.EvidenceRef{DocumentID: "doc-02", Page: 1, BlockID: "b2"},
						Extractor:        "liteparse",
						ExtractorVersion: "v3",
						DocType:          "POLICY_SCHEDULE",
					},
					{
						Value: "POL-Y", Normalized: "POL-Y",
						Evidence:         extract.EvidenceRef{DocumentID: "doc-01", Page: 1, BlockID: "b1"},
						Extractor:        "liteparse",
						ExtractorVersion: "v3",
						DocType:          "CLAIM_FORM",
					},
				},
			},
		},
	}
	result := verify.Result{
		Passed: false,
		Exceptions: []verify.Exception{
			{
				Code:        verify.CodePolicyNumberConflict,
				Severity:    verify.SeverityHigh,
				Message:     "Claim policy number does not match policy number.",
				EvidenceIDs: []string{"ev-doc-01"},
			},
		},
	}
	env, err := invest.Build(invest.BuildParams{
		TenantID:        tTenant,
		ClaimID:         tClaim,
		ExceptionID:     tExID,
		InvestigationID: tInvID,
		Result:          result,
		Claim:           claim,
		Unresolved:      unresolved,
		Evidence:        evidence,
		Scope: invest.ScopeConstraints{
			TenantID: tTenant, ClaimID: tClaim,
			AllowTools:   []invest.ToolName{invest.ToolGetClaim, invest.ToolGetDocuments, invest.ToolGetEvidence},
			MaxToolCalls: 5,
			DeadlineMs:   60000,
			RequestID:    tReqID,
		},
	})
	if err != nil {
		t.Fatalf("invest.Build: %v", err)
	}
	return env
}

// testScope derives a valid investigate.Scope echoing the envelope identity.
func testScope(env invest.UnresolvedException) investigate.Scope {
	return investigate.Scope{
		TenantID:   env.TenantID,
		ClaimID:    env.ClaimID,
		AllowTools: []invest.ToolName{invest.ToolGetClaim, invest.ToolGetDocuments, invest.ToolGetEvidence},
		MaxCalls:   5,
		DeadlineMs: 60000,
		RequestID:  env.Scope.RequestID,
	}
}

// successExecutor returns an executor whose tools succeed with canned IDs.
// Each tool returns one fresh ID so successful turns widen KnownEvidence.
func successExecutor() *investigate.Executor {
	return investigate.NewExecutor(map[invest.ToolName]investigate.ToolFunc{
		invest.ToolGetClaim: func(_ context.Context, req investigate.Request) (investigate.Response, error) {
			return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{"ev-new-01"}}, nil
		},
		invest.ToolGetEvidence: func(_ context.Context, req investigate.Request) (investigate.Response, error) {
			return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{"ev-new-02"}}, nil
		},
	}, time.Time{})
}

// failingExecutor returns an executor whose only tool fails with an
// upstream transient (investigate.ErrUpstream, which wraps
// ports.ErrUpstream, so errors.Is classifies at either layer). Execute
// retries it internally, then surfaces it; the loop records an UPSTREAM
// turn that widens KnownEvidence by nothing.
func failingExecutor() *investigate.Executor {
	return investigate.NewExecutor(map[invest.ToolName]investigate.ToolFunc{
		invest.ToolGetEvidence: func(_ context.Context, _ investigate.Request) (investigate.Response, error) {
			return investigate.Response{}, investigate.ErrUpstream
		},
	}, time.Time{})
}

// testReport builds a structurally valid, grounded report over the seed
// evidence IDs plus extraIDs (already-grown tool IDs). MissingAdditive
// echoes the envelope list verbatim so the additive-only check passes.
func testReport(env invest.UnresolvedException, extraIDs ...string) Report {
	evIDs := append([]string{"ev-doc-01"}, extraIDs...)
	slices.Sort(evIDs)
	return Report{
		Hypotheses: []invest.Hypothesis{
			{
				ID:        "h-01",
				Statement: "Policy number conflict stems from transcription variance.",
				Falsifier: "A pinned policy record showing the claimed number as active.",
				Status:    invest.HypothesisOpen,
				FactRefs: []invest.FactRef{
					{Key: "hospital_name", Agreed: "City Hospital", EvidenceID: "ev-doc-02"},
				},
				EvidenceIDs: evIDs,
			},
		},
		Findings: []invest.Finding{
			{ID: "f-01", HypothesisID: "h-01", Summary: "Cited evidence shows the conflict.", EvidenceIDs: evIDs},
		},
		Recommendation: invest.Recommendation{
			Action:     invest.RecommendReferHuman,
			Rationale:  "Needs human review.",
			FindingIDs: []string{"f-01"},
		},
		MissingAdditive: append([]invest.MissingItem(nil), env.MissingEvidence...),
	}
}

// callToolBytes renders one valid call_tool act for tool at limit.
func callToolBytes(t *testing.T, env invest.UnresolvedException, scope investigate.Scope, tool invest.ToolName, limit int) []byte {
	t.Helper()
	req, err := investigate.NewRequest(tool, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, limit)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: tool, Request: &req})
	if err != nil {
		t.Fatalf("marshal call_tool: %v", err)
	}
	return raw
}

// submitBytes renders one submit_report act.
func submitBytes(t *testing.T, r Report) []byte {
	t.Helper()
	raw, err := json.Marshal(ModelAction{Action: ActionSubmitReport, Report: &r})
	if err != nil {
		t.Fatalf("marshal submit_report: %v", err)
	}
	return raw
}

// withUnknownField returns a copy of a valid act with one extra unknown
// field spliced before the final brace, so strict decoding rejects it
// (I1-malformed). The input is never aliased: the splice works on a copy.
func withUnknownField(t *testing.T, raw []byte, field string) []byte {
	t.Helper()
	if !json.Valid(raw) {
		t.Fatalf("base payload is not valid JSON")
	}
	if len(raw) == 0 || raw[len(raw)-1] != '}' {
		t.Fatalf("base payload does not end in }")
	}
	out := append(append([]byte(nil), raw[:len(raw)-1]...), []byte(","+field+"}")...)
	if !json.Valid(out) {
		t.Fatalf("spliced payload is not valid JSON")
	}
	return out
}

// newTestLoop builds a loop with default budgets, failing the test on any
// construction error.
func newTestLoop(t *testing.T, model ModelClient, exec *investigate.Executor, scope investigate.Scope, env invest.UnresolvedException) *Loop {
	t.Helper()
	lp, err := NewLoop(model, exec, DefaultBudgets(scope), scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	return lp
}

// ---------------------------------------------------------------------------
// Happy path + repair
// ---------------------------------------------------------------------------

func TestHappySubmit(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Outcome != OutcomeReportReady {
		t.Fatalf("Outcome = %q, want REPORT_READY", out.Outcome)
	}
	if out.Report == nil {
		t.Fatal("REPORT_READY without a report")
	}
	if out.TurnsUsed != 2 || out.ToolCallsUsed != 1 || len(out.AttemptLog) != 1 {
		t.Fatalf("TurnsUsed=%d ToolCallsUsed=%d AttemptLog=%d, want 2/1/1",
			out.TurnsUsed, out.ToolCallsUsed, len(out.AttemptLog))
	}
	if fake.Calls != 2 {
		t.Fatalf("model calls = %d, want 2", fake.Calls)
	}
	if err := ValidateInvestigationOutput(out); err != nil {
		t.Fatalf("ValidateInvestigationOutput: %v", err)
	}
}

func TestMalformedThenRepairThenSubmit(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	bad := withUnknownField(t, callToolBytes(t, env, scope, invest.ToolGetClaim, 1), `"bogus":1`)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(bad), // I1-malformed: earns the single same-turn re-prompt.
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Outcome != OutcomeReportReady {
		t.Fatalf("Outcome = %q, want REPORT_READY after repair", out.Outcome)
	}
	if fake.Calls != 3 {
		t.Fatalf("model calls = %d, want 3 (invalid + repair + submit)", fake.Calls)
	}
	if out.TurnsUsed != 2 {
		t.Fatalf("TurnsUsed = %d, want 2 (re-prompt consumes no turn)", out.TurnsUsed)
	}
}

// ---------------------------------------------------------------------------
// Invalid-output escalations
// ---------------------------------------------------------------------------

func TestGroundingRejectDanglingFinding(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	rep := testReport(env)
	rep.Findings[0].HypothesisID = "h-nope" // structurally valid, ungrounded.
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(submitBytes(t, rep))}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want INVALID_OUTPUT escalation")
	}
	if !errors.Is(err, ErrGrounding) {
		t.Fatalf("err = %v, want ErrGrounding", err)
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
	}
	if out.Partial == nil {
		t.Fatal("grounding rejection of a structural report should carry Partial progress")
	}
}

func TestSecondInvalidEscalates(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	bad := withUnknownField(t, callToolBytes(t, env, scope, invest.ToolGetClaim, 1), `"bogus":1`)
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(bad), modelResp(bad)}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want INVALID_OUTPUT escalation")
	}
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want ErrModelContract", err)
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
	}
	if fake.Calls != 2 {
		t.Fatalf("model calls = %d, want 2 (initial + one re-prompt)", fake.Calls)
	}
}

// ---------------------------------------------------------------------------
// Repetition + no-progress
// ---------------------------------------------------------------------------

func TestExactRepeatEscalates(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	call := callToolBytes(t, env, scope, invest.ToolGetClaim, 1)
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(call), modelResp(call)}}
	exec := successExecutor()
	out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want REPETITION escalation")
	}
	if !errors.Is(err, ErrRepetition) {
		t.Fatalf("err = %v, want ErrRepetition", err)
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationRepetition {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/REPETITION", out.Outcome, out.EscalationReason)
	}
	if out.ToolCallsUsed != 1 {
		t.Fatalf("ToolCallsUsed = %d, want 1 (repeat rejected before Execute)", out.ToolCallsUsed)
	}
}

func TestNoProgressAfterStagnantTurns(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	// Failing tools record UPSTREAM turns that widen KnownEvidence by
	// nothing; three consecutive stagnant turns escalate NO_PROGRESS.
	// Limits differ per turn so the repetition check does not fire first.
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 1)),
		modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 2)),
		modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 3)),
	}}
	exec := failingExecutor()
	out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want NO_PROGRESS escalation")
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationNoProgress {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/NO_PROGRESS", out.Outcome, out.EscalationReason)
	}
	if len(out.AttemptLog) != 3 {
		t.Fatalf("AttemptLog = %d turns, want 3 stagnant turns", len(out.AttemptLog))
	}
	for i, rec := range out.AttemptLog {
		if rec.ErrorCode != "UPSTREAM" {
			t.Fatalf("AttemptLog[%d].ErrorCode = %q, want UPSTREAM", i, rec.ErrorCode)
		}
	}
}

// ---------------------------------------------------------------------------
// Budgets + deadline
// ---------------------------------------------------------------------------

func TestTurnsExhaustion(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	b := DefaultBudgets(scope)
	b.MaxTurns = 1
	b.MaxModelCalls = 1
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(submitBytes(t, testReport(env, "ev-new-01"))), // extra turn: never reached.
	}}
	lp, err := NewLoop(fake, successExecutor(), b, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want TURNS_EXHAUSTED escalation")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationTurnsExhausted {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/TURNS_EXHAUSTED", out.Outcome, out.EscalationReason)
	}
	if fake.Calls != 1 {
		t.Fatalf("model calls = %d, want 1 (extra scripted turn unused)", fake.Calls)
	}
}

func TestModelCallsExhaustion(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	b := DefaultBudgets(scope)
	b.MaxTurns = 4
	b.MaxModelCalls = 4 // bad+repair (2) + two distinct doc calls (2); a fifth call has none left.
	bad := withUnknownField(t, callToolBytes(t, env, scope, invest.ToolGetClaim, 1), `"bogus":1`)
	threeExec := investigate.NewExecutor(map[invest.ToolName]investigate.ToolFunc{
		invest.ToolGetClaim: func(_ context.Context, req investigate.Request) (investigate.Response, error) {
			return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{"ev-new-01"}}, nil
		},
		invest.ToolGetDocuments: func(_ context.Context, req investigate.Request) (investigate.Response, error) {
			return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{"ev-new-02"}}, nil
		},
		invest.ToolGetEvidence: func(_ context.Context, req investigate.Request) (investigate.Response, error) {
			return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{"ev-new-03"}}, nil
		},
	}, time.Time{})
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(bad),
		// Distinct calls keep the repetition guard quiet so
		// call-count exhaustion is what fires.
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(callToolBytes(t, env, scope, invest.ToolGetDocuments, 1)),
		modelResp(callToolBytes(t, env, scope, invest.ToolGetDocuments, 2)),
		modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 1)),
	}}
	lp, err := NewLoop(fake, threeExec, b, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want CALLS_EXHAUSTED escalation")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if out.EscalationReason != EscalationCallsExhausted {
		t.Fatalf("Reason = %q, want CALLS_EXHAUSTED", out.EscalationReason)
	}
}

func TestToolCallsBudgetEnforced(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	scope.MaxCalls = 2
	b := DefaultBudgets(scope) // MaxToolCalls follows scope: 2.
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 1)),
		modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 2)),
	}}
	exec := successExecutor()
	lp, err := NewLoop(fake, exec, b, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want CALLS_EXHAUSTED escalation")
	}
	if out.EscalationReason != EscalationCallsExhausted {
		t.Fatalf("Reason = %q, want CALLS_EXHAUSTED", out.EscalationReason)
	}
	if out.ToolCallsUsed != 2 {
		t.Fatalf("ToolCallsUsed = %d, want 2 (budget cap)", out.ToolCallsUsed)
	}
	if exec.Calls() != 2 {
		t.Fatalf("executor Calls() = %d, want 2", exec.Calls())
	}
}

func TestDeadlineEscalates(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	scope.DeadlineMs = 100
	// The first Complete outlives the 100ms scope deadline, so the turn 1
	// mid-turn re-check fires after Complete returns and before Execute.
	// TurnTimeoutMs stays generous so the turn context itself does not
	// fire first.
	fake := &FakeModelClient{
		Delay: 250 * time.Millisecond,
		Responses: []ModelResponse{
			modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
			modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
		},
	}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want DEADLINE escalation")
	}
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want ErrDeadlineExceeded", err)
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationDeadline {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/DEADLINE", out.Outcome, out.EscalationReason)
	}
}

// ---------------------------------------------------------------------------
// Tenant isolation
// ---------------------------------------------------------------------------

func TestTenantMismatchAtConstruction(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	scope.TenantID = "tnt-other"
	fake := &FakeModelClient{}
	if _, err := NewLoop(fake, successExecutor(), DefaultBudgets(scope), scope, env, nil); err == nil {
		t.Fatal("NewLoop succeeded, want F6 tenant abort")
	} else if !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("err = %v, want ErrTenantMismatch", err)
	}
}

func TestTenantMismatchMidRun(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
	}}
	lp := newTestLoop(t, fake, successExecutor(), scope, env)
	lp.scope.TenantID = "tnt-other" // simulate a mid-run scope split (F6).
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want F6 tenant abort")
	}
	if !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("err = %v, want ErrTenantMismatch", err)
	}
	if out.Outcome != "" {
		t.Fatalf("Outcome = %q, want empty output on tenant abort", out.Outcome)
	}
	if fake.Calls != 0 {
		t.Fatalf("model calls = %d, want 0 (abort before first Complete)", fake.Calls)
	}
}

// ---------------------------------------------------------------------------
// Forbidden-field + strict-decode corpora
// ---------------------------------------------------------------------------

func TestForbiddenFieldCorpus(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	validCall := callToolBytes(t, env, scope, invest.ToolGetClaim, 1)

	approveRep := testReport(env)
	approveRep.Recommendation.Action = "APPROVE" // verdict vocabulary: I5-report.
	approvePayload, err := json.Marshal(ModelAction{Action: ActionSubmitReport, Report: &approveRep})
	if err != nil {
		t.Fatalf("marshal approve payload: %v", err)
	}

	cases := []struct {
		name    string
		payload []byte
	}{
		{"confidence", withUnknownField(t, validCall, `"confidence":0.9`)},
		{"score", withUnknownField(t, validCall, `"score":0.1`)},
		{"agreed_raw", withUnknownField(t, validCall, `"agreed_raw":"City Hospital"`)},
		{"approve", approvePayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &FakeModelClient{Responses: []ModelResponse{
				modelResp(tc.payload),
				modelResp(tc.payload), // second copy: repromptable classes escalate here.
			}}
			out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
			if err == nil {
				t.Fatalf("%s payload accepted, want INVALID_OUTPUT", tc.name)
			}
			if !errors.Is(err, ErrModelContract) {
				t.Fatalf("%s err = %v, want ErrModelContract", tc.name, err)
			}
			if out.EscalationReason != EscalationInvalidOutput {
				t.Fatalf("%s Reason = %q, want INVALID_OUTPUT", tc.name, out.EscalationReason)
			}
		})
	}
}

func TestStrictDecodeCorpus(t *testing.T) {
	env := testEnvelope(t)
	validAct := submitBytes(t, testReport(env))

	t.Run("model trailing data rejected", func(t *testing.T) {
		withTrailing := append(append([]byte(nil), validAct...), []byte(` {}`)...)
		if _, err := DecodeModelAction(withTrailing, DefaultMaxOutputBytes); err == nil {
			t.Fatal("trailing data accepted, want rejection")
		}
	})
	t.Run("model unknown fields rejected", func(t *testing.T) {
		withUnknown := withUnknownField(t, validAct, `"confidence":0.9`)
		if _, err := DecodeModelAction(withUnknown, DefaultMaxOutputBytes); err == nil {
			t.Fatal("unknown field accepted, want rejection")
		}
	})
	t.Run("model empty rejected", func(t *testing.T) {
		if _, err := DecodeModelAction([]byte("   "), DefaultMaxOutputBytes); !errors.Is(err, ErrModelEmpty) {
			t.Fatalf("err = %v, want ErrModelEmpty", err)
		}
	})
	t.Run("model oversize rejected", func(t *testing.T) {
		if _, err := DecodeModelAction(validAct, 10); err == nil {
			t.Fatal("oversize payload accepted, want rejection")
		}
	})

	validOut := InvestigationOutput{
		InvestigationID: tInvID,
		Outcome:         OutcomeReportReady,
		Report:          func() *Report { r := testReport(env); return &r }(),
		AttemptLog:      []TurnRecord{},
		TurnsUsed:       0,
		ToolCallsUsed:   0,
	}
	rawOut, err := MarshalOutput(validOut)
	if err != nil {
		t.Fatalf("MarshalOutput: %v", err)
	}
	t.Run("output trailing data rejected", func(t *testing.T) {
		withTrailing := append(append([]byte(nil), rawOut...), []byte(` {}`)...)
		if _, err := DecodeOutput(withTrailing); err == nil {
			t.Fatal("trailing data accepted, want rejection")
		}
	})
	t.Run("output unknown fields rejected", func(t *testing.T) {
		withUnknown := withUnknownField(t, rawOut, `"bogus":1`)
		if _, err := DecodeOutput(withUnknown); err == nil {
			t.Fatal("unknown field accepted, want rejection")
		}
	})
	t.Run("output round trip stable", func(t *testing.T) {
		back, err := DecodeOutput(rawOut)
		if err != nil {
			t.Fatalf("DecodeOutput: %v", err)
		}
		again, err := MarshalOutput(back)
		if err != nil {
			t.Fatalf("MarshalOutput: %v", err)
		}
		if !bytes.Equal(rawOut, again) {
			t.Fatal("decode/re-marshal changed the canonical bytes")
		}
	})
}

// TestWireVocabularyScan asserts the float/confidence vocabulary never
// appears as a key in any marshaled wire type.
func TestWireVocabularyScan(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	forbidden := []string{"confidence", "score", "probability"}

	req := ModelRequest{
		Exception:        env,
		History:          []TurnRecord{},
		KnownEvidenceIDs: KnownIDs(mustSeed(t, env)),
		Turn:             1,
		RequestID:        scope.RequestID,
	}
	rawReq, err := CanonicalModelRequest(req)
	if err != nil {
		t.Fatalf("CanonicalModelRequest: %v", err)
	}
	rep := testReport(env, "ev-new-01")
	rawAct := submitBytes(t, rep)
	out := InvestigationOutput{
		InvestigationID: tInvID,
		Outcome:         OutcomeReportReady,
		Report:          &rep,
		AttemptLog:      []TurnRecord{},
		TurnsUsed:       0,
		ToolCallsUsed:   0,
	}
	rawOut, err := MarshalOutput(out)
	if err != nil {
		t.Fatalf("MarshalOutput: %v", err)
	}
	for _, raw := range [][]byte{rawReq, rawAct, rawOut} {
		keys := jsonKeys(t, raw)
		for _, k := range keys {
			for _, f := range forbidden {
				if strings.EqualFold(k, f) {
					t.Fatalf("wire JSON carries forbidden key %q", k)
				}
			}
		}
	}
}

func mustSeed(t *testing.T, env invest.UnresolvedException) KnownEvidence {
	t.Helper()
	k, err := SeedKnownEvidence(env)
	if err != nil {
		t.Fatalf("SeedKnownEvidence: %v", err)
	}
	return k
}

// jsonKeys collects every object key in a JSON document, recursively.
func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal for key scan: %v", err)
	}
	var out []string
	var walk func(x any)
	walk = func(x any) {
		switch n := x.(type) {
		case map[string]any:
			for k, val := range n {
				out = append(out, k)
				walk(val)
			}
		case []any:
			for _, e := range n {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// ---------------------------------------------------------------------------
// Stability + replay
// ---------------------------------------------------------------------------

func TestMarshalOutputStability(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	first, err := MarshalOutput(out)
	if err != nil {
		t.Fatalf("MarshalOutput: %v", err)
	}
	second, err := MarshalOutput(out)
	if err != nil {
		t.Fatalf("MarshalOutput: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("same output marshaled to different bytes")
	}
}

func runScripted(t *testing.T) []byte {
	t.Helper()
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := MarshalOutput(out)
	if err != nil {
		t.Fatalf("MarshalOutput: %v", err)
	}
	return raw
}

func TestReplayByteIdentical(t *testing.T) {
	first := runScripted(t)
	second := runScripted(t)
	if !bytes.Equal(first, second) {
		t.Fatal("same scripted sequence produced different bytes")
	}
}

// ---------------------------------------------------------------------------
// Unauthorized tool proposals (T11 + out-of-scope)
// ---------------------------------------------------------------------------

func TestUnauthorizedToolProposalDenied(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)

	t.Run("t11 writer never callable", func(t *testing.T) {
		raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolCreateInvestigationReport})
		if err != nil {
			t.Fatalf("marshal t11 act: %v", err)
		}
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw), modelResp(raw)}}
		exec := successExecutor()
		out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
		if err == nil {
			t.Fatal("Run succeeded, want INVALID_OUTPUT escalation")
		}
		if !errors.Is(err, ErrToolDenied) {
			t.Fatalf("err = %v, want ErrToolDenied", err)
		}
		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
			t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
		}
		if exec.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0 (denied before Execute)", exec.Calls())
		}
		if fake.Calls != 1 {
			t.Fatalf("model calls = %d, want 1 (denied class never re-prompts)", fake.Calls)
		}
	})

	t.Run("out of scope tool denied", func(t *testing.T) {
		req, err := investigate.NewRequest(invest.ToolSearchEvidence, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, 1)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Query = "policy" // allowlisted globally, but absent from scope.AllowTools.
		raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolSearchEvidence, Request: &req})
		if err != nil {
			t.Fatalf("marshal out-of-scope act: %v", err)
		}
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw), modelResp(raw)}}
		exec := successExecutor()
		out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
		if err == nil {
			t.Fatal("Run succeeded, want INVALID_OUTPUT escalation")
		}
		if !errors.Is(err, ErrToolDenied) {
			t.Fatalf("err = %v, want ErrToolDenied", err)
		}
		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
			t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
		}
		if exec.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0 (denied before Execute)", exec.Calls())
		}
	})
}

// ---------------------------------------------------------------------------
// Model upstream retry
// ---------------------------------------------------------------------------

func TestModelUpstreamRetryThenRepair(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	call := callToolBytes(t, env, scope, invest.ToolGetClaim, 1)
	submit := submitBytes(t, testReport(env, "ev-new-01"))
	// Responses[0] is never returned: call 0 fails upstream, the same-turn
	// retry serves Responses[1], and turn 2 serves Responses[2].
	fake := &FakeModelClient{
		Errs:      []error{ErrModelUpstream},
		Responses: []ModelResponse{modelResp(call), modelResp(call), modelResp(submit)},
	}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Outcome != OutcomeReportReady {
		t.Fatalf("Outcome = %q, want REPORT_READY after retry", out.Outcome)
	}
	if fake.Calls != 3 {
		t.Fatalf("model calls = %d, want 3 (fail + retry + submit)", fake.Calls)
	}
	if out.ToolCallsUsed != 1 {
		t.Fatalf("ToolCallsUsed = %d, want 1", out.ToolCallsUsed)
	}
}

func TestModelDoubleUpstreamEscalates(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	call := callToolBytes(t, env, scope, invest.ToolGetClaim, 1)
	fake := &FakeModelClient{
		Errs:      []error{ErrModelUpstream, ErrModelUpstream},
		Responses: []ModelResponse{modelResp(call)},
	}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want MODEL_UPSTREAM escalation")
	}
	if !errors.Is(err, ErrModelUpstream) {
		t.Fatalf("err = %v, want ErrModelUpstream", err)
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationModelUpstream {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/MODEL_UPSTREAM", out.Outcome, out.EscalationReason)
	}
	if fake.Calls != 2 {
		t.Fatalf("model calls = %d, want 2 (initial + one retry)", fake.Calls)
	}
}

// ---------------------------------------------------------------------------
// Model request capture
// ---------------------------------------------------------------------------

// capturingModelClient records every ModelRequest it receives and serves one
// scripted response per call.
type capturingModelClient struct {
	reqs  []ModelRequest
	resps []ModelResponse
	calls int
}

func (c *capturingModelClient) Complete(_ context.Context, req ModelRequest) (ModelResponse, error) {
	c.reqs = append(c.reqs, req)
	i := c.calls
	c.calls++
	if i < len(c.resps) {
		return c.resps[i], nil
	}
	return c.resps[len(c.resps)-1], nil
}

func TestModelRequestCapture(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &capturingModelClient{resps: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Outcome != OutcomeReportReady {
		t.Fatalf("Outcome = %q, want REPORT_READY", out.Outcome)
	}
	if len(fake.reqs) != 2 {
		t.Fatalf("captured requests = %d, want 2", len(fake.reqs))
	}
	if fake.reqs[0].Turn != 1 || fake.reqs[1].Turn != 2 {
		t.Fatalf("turns = [%d %d], want [1 2]", fake.reqs[0].Turn, fake.reqs[1].Turn)
	}
	for i, req := range fake.reqs {
		if req.RequestID != scope.RequestID {
			t.Fatalf("req[%d].RequestID = %q, want scope %q", i, req.RequestID, scope.RequestID)
		}
		if !slices.IsSorted(req.KnownEvidenceIDs) {
			t.Fatalf("req[%d].KnownEvidenceIDs not sorted: %q", i, req.KnownEvidenceIDs)
		}
	}
	if len(fake.reqs[0].History) != 0 {
		t.Fatalf("req[0] history = %d records, want 0", len(fake.reqs[0].History))
	}
	if len(fake.reqs[1].History) != 1 {
		t.Fatalf("req[1] history = %d records, want 1", len(fake.reqs[1].History))
	} else if fake.reqs[1].History[0].Turn != 1 {
		t.Fatalf("req[1] history turn = %d, want 1", fake.reqs[1].History[0].Turn)
	}
	if !slices.Contains(fake.reqs[1].KnownEvidenceIDs, "ev-new-01") {
		t.Fatalf("req[1] KnownEvidenceIDs = %q, want grown id ev-new-01", fake.reqs[1].KnownEvidenceIDs)
	}
	if len(fake.reqs[1].KnownEvidenceIDs) != len(fake.reqs[0].KnownEvidenceIDs)+1 {
		t.Fatalf("known ids did not grow by exactly one: %q -> %q",
			fake.reqs[0].KnownEvidenceIDs, fake.reqs[1].KnownEvidenceIDs)
	}
}

// ---------------------------------------------------------------------------
// Context cancellation + input cap
// ---------------------------------------------------------------------------

func TestContextCancelEmitsNoAuditRow(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
	}}
	var got []LoopAuditEvent
	hook := func(_ context.Context, e LoopAuditEvent) { got = append(got, e) }
	lp, err := NewLoop(fake, successExecutor(), DefaultBudgets(scope), scope, env, hook)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := lp.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want raw context.Canceled", err)
	}
	if out.Outcome != "" {
		t.Fatalf("Outcome = %q, want empty output on ctx cancel", out.Outcome)
	}
	if len(got) != 0 {
		t.Fatalf("audit hook recorded %d rows, want 0 (no started, no finished)", len(got))
	}
	if fake.Calls != 0 {
		t.Fatalf("model calls = %d, want 0 (abort before first Complete)", fake.Calls)
	}
}

func TestMaxInputBytesEscalates(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	b := DefaultBudgets(scope)
	b.MaxInputBytes = 1 // every canonical request overshoots: history never fits.
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
	}}
	lp, err := NewLoop(fake, successExecutor(), b, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want INVALID_OUTPUT escalation")
	}
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want ErrModelContract", err)
	}
	if invalidKindOf(err) != invalidOversize {
		t.Fatalf("invalid kind = %s, want I8-oversize", invalidKindString(invalidKindOf(err)))
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
	}
	if fake.Calls != 0 {
		t.Fatalf("model calls = %d, want 0 (cap checked before Complete)", fake.Calls)
	}
}

// ---------------------------------------------------------------------------
// Audit hook lifecycle
// ---------------------------------------------------------------------------

func TestAuditHookLifecycle(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)

	t.Run("happy path started and finished", func(t *testing.T) {
		fake := &FakeModelClient{Responses: []ModelResponse{
			modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
			modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
		}}
		var got []LoopAuditEvent
		hook := func(_ context.Context, e LoopAuditEvent) { got = append(got, e) }
		lp, err := NewLoop(fake, successExecutor(), DefaultBudgets(scope), scope, env, hook)
		if err != nil {
			t.Fatalf("NewLoop: %v", err)
		}
		if _, err := lp.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("audit rows = %d, want 2 (started + finished)", len(got))
		}
		if got[0].Event != AuditEventStarted || got[1].Event != AuditEventFinished {
			t.Fatalf("events = [%q %q], want [started finished]", got[0].Event, got[1].Event)
		}
		if got[1].Outcome != string(OutcomeReportReady) || got[1].Reason != "" {
			t.Fatalf("finished Outcome=%q Reason=%q, want REPORT_READY/empty", got[1].Outcome, got[1].Reason)
		}
		for i, e := range got {
			if e.InvestigationID != tInvID || e.RequestID != scope.RequestID {
				t.Fatalf("row[%d] ids = %q/%q, want %q/%q", i, e.InvestigationID, e.RequestID, tInvID, scope.RequestID)
			}
			if err := ValidateLoopAuditEvent(e); err != nil {
				t.Fatalf("row[%d] invalid: %v", i, err)
			}
		}
	})

	t.Run("escalation path started and finished", func(t *testing.T) {
		bad := withUnknownField(t, callToolBytes(t, env, scope, invest.ToolGetClaim, 1), `"bogus":1`)
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(bad), modelResp(bad)}}
		var got []LoopAuditEvent
		hook := func(_ context.Context, e LoopAuditEvent) { got = append(got, e) }
		lp, err := NewLoop(fake, successExecutor(), DefaultBudgets(scope), scope, env, hook)
		if err != nil {
			t.Fatalf("NewLoop: %v", err)
		}
		out, err := lp.Run(context.Background())
		if err == nil {
			t.Fatal("Run succeeded, want INVALID_OUTPUT escalation")
		}
		if len(got) != 2 {
			t.Fatalf("audit rows = %d, want 2 (started + finished)", len(got))
		}
		if got[0].Event != AuditEventStarted || got[1].Event != AuditEventFinished {
			t.Fatalf("events = [%q %q], want [started finished]", got[0].Event, got[1].Event)
		}
		if got[1].Outcome != string(OutcomeEscalated) || got[1].Reason != string(EscalationInvalidOutput) {
			t.Fatalf("finished Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", got[1].Outcome, got[1].Reason)
		}
		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
			t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
		}
	})
}

// ---------------------------------------------------------------------------
// Grounding holes
// ---------------------------------------------------------------------------

func TestGroundingRejectsUnknownEvidenceID(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	rep := testReport(env)
	// ev-doc-99 is structurally citable (sorted, unique) but never seeded
	// and never grown: Gate B must reject it as invented provenance.
	rep.Hypotheses[0].EvidenceIDs = []string{"ev-doc-01", "ev-doc-99"}
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(submitBytes(t, rep))}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want INVALID_OUTPUT escalation")
	}
	if !errors.Is(err, ErrGrounding) {
		t.Fatalf("err = %v, want ErrGrounding", err)
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
	}
	if out.Partial == nil {
		t.Fatal("grounding rejection of a structural report should carry Partial progress")
	}
}

func TestCrossTenantRequestIDRejected(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	req, err := investigate.NewRequest(invest.ToolGetClaim, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, 1)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.TenantID = "tnt-other" // well-formed but outside the run scope (I4).
	raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolGetClaim, Request: &req})
	if err != nil {
		t.Fatalf("marshal cross-tenant act: %v", err)
	}
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw), modelResp(raw)}}
	exec := successExecutor()
	out, err := newTestLoop(t, fake, exec, scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want INVALID_OUTPUT escalation")
	}
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want ErrModelContract", err)
	}
	if invalidKindOf(err) != invalidRequest {
		t.Fatalf("invalid kind = %s, want I4-request", invalidKindString(invalidKindOf(err)))
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
	}
	if exec.Calls() != 0 {
		t.Fatalf("executor Calls() = %d, want 0 (rejected before Execute)", exec.Calls())
	}
}

// ---------------------------------------------------------------------------
// Deterministic-failure poisoning + no-progress cause chain
// ---------------------------------------------------------------------------

func TestDeterministicFailurePoisonsRepetition(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	call := callToolBytes(t, env, scope, invest.ToolGetClaim, 1)
	// A terminal (non-upstream) tool failure poisons the repetition set:
	// the identical second act observes nothing new and escalates.
	exec := investigate.NewExecutor(map[invest.ToolName]investigate.ToolFunc{
		invest.ToolGetClaim: func(_ context.Context, _ investigate.Request) (investigate.Response, error) {
			return investigate.Response{}, investigate.ErrContract
		},
	}, time.Time{})
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(call), modelResp(call)}}
	lp, err := NewLoop(fake, exec, DefaultBudgets(scope), scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want REPETITION escalation")
	}
	if !errors.Is(err, ErrRepetition) {
		t.Fatalf("err = %v, want ErrRepetition", err)
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationRepetition {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/REPETITION", out.Outcome, out.EscalationReason)
	}
	if out.ToolCallsUsed != 1 {
		t.Fatalf("ToolCallsUsed = %d, want 1 (repeat rejected before Execute)", out.ToolCallsUsed)
	}
	if exec.Calls() != 1 {
		t.Fatalf("executor Calls() = %d, want 1", exec.Calls())
	}
}

func TestNoProgressCarriesUpstreamCause(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	// Limits differ per turn so the repetition check does not fire first;
	// every turn records an UPSTREAM failure that widens nothing.
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 1)),
		modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 2)),
		modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 3)),
	}}
	out, err := newTestLoop(t, fake, failingExecutor(), scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want NO_PROGRESS escalation")
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationNoProgress {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/NO_PROGRESS", out.Outcome, out.EscalationReason)
	}
	if !errors.Is(err, investigate.ErrUpstream) {
		t.Fatalf("err = %v, want cause chain to carry investigate.ErrUpstream", err)
	}
}
