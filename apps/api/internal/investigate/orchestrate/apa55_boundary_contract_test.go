package orchestrate

// APA-55 deterministic boundary contract: tool-use gates and output-contract
// rejection, extracted from the APA-55 Phase 2 qualification run.
//
// PROVENANCE. The source artifact is
// apa55_phase2_qualification_live_test.go, an untracked APA-55 qualification
// RUN file that also carried two live halves — TestAPA55_A_Evidence and
// TestAPA55_B_Live. Those live halves SKIP unconditionally without provider
// credentials and are superseded by the corrected live layer already tracked
// in apa55_live_qualification_test.go. This file carries forward ONLY the
// credential-free halves, which measured unique boundary coverage: if the
// untracked artifact were lost, ClaimOps would silently drop these assertions
// and CI would stay green. Tracked presence is the point.
//
// SCOPE. Both tests are fully deterministic and run with FakeModelClient
// through the real Loop, or call the decoder/validator directly. No live
// provider call, no network, no wall-clock dependency, no Postgres.
//
// Acceptance rule (inherited from the qualification run): a case PASSES when
// the deterministic boundary fails closed — the invalid or adversarial model
// output is rejected at the decoder/ownership/grounding/tenant boundary with
// zero unauthorized tool execution and zero fabricated accepted evidence. A
// case FAILS only when an invalid output becomes a persisted state, a
// cross-tenant leak, an unauthorized action, or an accepted fabricated claim.
//
// Fidelity: subtest names, assertions, expected outcomes, and the "APA55-B" /
// "APA55-C" log prefixes are preserved verbatim from the source artifact so
// messages stay comparable across the extraction. Only the two top-level test
// function names changed, to avoid colliding with the untouched provenance
// artifact. No assertion was weakened, added, or reordered.
//
// Harness: orchestrate_test.go fixtures (testEnvelope, testScope,
// successExecutor, failingExecutor, testReport, callToolBytes, submitBytes,
// withUnknownField, modelResp, newTestLoop) plus FakeModelClient in
// mock_model.go. No helper is defined here, so this file is independent of
// the live harness helpers used by the retired halves.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// ---------------------------------------------------------------------------
// Group C: output contract — deterministic decoder/validator rejection
// ---------------------------------------------------------------------------

// TestAPA55_Boundary_OutputContract (was TestAPA55_C_OutputContract) feeds
// model-shaped payloads that are wrong in exactly one way and asserts each is
// rejected at the deterministic boundary. Deterministic cases: a single honest
// measurement each.
func TestAPA55_Boundary_OutputContract(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	valid := callToolBytes(t, env, scope, invest.ToolGetEvidence, 1)

	t.Run("malformed_json", func(t *testing.T) {
		_, err := DecodeModelAction([]byte(`{"act":"call_tool","tool":`), 1<<20)
		if err == nil {
			t.Fatal("malformed JSON accepted")
		}
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		t.Logf("APA55-C malformed_json rejected: %v", err)
	})

	t.Run("unknown_field_value", func(t *testing.T) {
		bad := withUnknownField(t, valid, `"value":"x"`)
		_, err := DecodeModelAction(bad, 1<<20)
		if err == nil || !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		t.Logf("APA55-C unknown_field_value rejected: %v", err)
	})

	t.Run("unknown_field_description", func(t *testing.T) {
		bad := withUnknownField(t, valid, `"description":"x"`)
		_, err := DecodeModelAction(bad, 1<<20)
		if err == nil || !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		t.Logf("APA55-C unknown_field_description rejected: %v", err)
	})

	t.Run("unknown_field_reason", func(t *testing.T) {
		bad := withUnknownField(t, valid, `"reason":"x"`)
		_, err := DecodeModelAction(bad, 1<<20)
		if err == nil || !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		t.Logf("APA55-C unknown_field_reason rejected: %v", err)
	})

	t.Run("wrong_enum_manual_review", func(t *testing.T) {
		// A report whose recommendation action is not in the enum.
		r := testReport(env)
		r.Recommendation.Action = "manual_review"
		raw, err := json.Marshal(ModelAction{Action: ActionSubmitReport, Report: &r})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		a, err := DecodeModelAction(raw, 1<<20)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := ValidateModelAction(a, scope, env.InvestigationID); err == nil {
			t.Fatal("wrong enum accepted")
		} else {
			t.Logf("APA55-C wrong_enum_manual_review rejected at ValidateModelAction: %v", err)
		}
	})

	t.Run("missing_required_field", func(t *testing.T) {
		// A call_tool act missing the request field.
		raw := []byte(`{"action":"call_tool","tool":"get_evidence"}`)
		a, err := DecodeModelAction(raw, 1<<20)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := ValidateModelAction(a, scope, env.InvestigationID); err == nil {
			t.Fatal("missing required field accepted")
		} else {
			t.Logf("APA55-C missing_required_field rejected at ValidateModelAction: %v", err)
		}
	})

	t.Run("null_where_forbidden", func(t *testing.T) {
		// A call_tool act with null request.
		raw := []byte(`{"action":"call_tool","tool":"get_evidence","request":null}`)
		a, err := DecodeModelAction(raw, 1<<20)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := ValidateModelAction(a, scope, env.InvestigationID); err == nil {
			t.Fatal("null where forbidden accepted")
		} else {
			t.Logf("APA55-C null_where_forbidden rejected at ValidateModelAction: %v", err)
		}
	})

	t.Run("wrong_type", func(t *testing.T) {
		// limit as a string instead of int.
		raw := []byte(`{"action":"call_tool","tool":"get_evidence","request":{"tool":"get_evidence","tenant_id":"t","claim_id":"c","investigation_id":"i","request_id":"r","limit":"5","cursor":"","query":"","subject_id":"","source_type":"","hash":"","payload":null}}`)
		_, err := DecodeModelAction(raw, 1<<20)
		if err == nil {
			t.Fatal("wrong type accepted")
		}
		t.Logf("APA55-C wrong_type rejected at DecodeModelAction: %v", err)
	})

	t.Run("duplicate_keys", func(t *testing.T) {
		raw := []byte(`{"action":"call_tool","action":"submit_report","tool":"get_evidence","request":{"tool":"get_evidence","tenant_id":"t","claim_id":"c","investigation_id":"i","request_id":"r","limit":5,"cursor":"","query":"\"\",\"subject_id\":\"\",\"source_type\":\"\",\"hash":"","payload":null}}`)
		_, err := DecodeModelAction(raw, 1<<20)
		// Go encoding/json v1 does NOT reject duplicate keys: it takes the
		// last value. This is a measured limitation of the decoder boundary,
		// not a leak — the resulting action is still validated by
		// ValidateModelAction, so a duplicate-key payload cannot smuggle an
		// unvalidated action through.
		if err == nil {
			t.Logf("APA55-C duplicate_keys: decoder ACCEPTS (Go json v1 last-wins); action still validated downstream — measured limitation, not a leak")
		} else {
			t.Logf("APA55-C duplicate_keys rejected at DecodeModelAction: %v", err)
		}
	})

	t.Run("semantically_invalid_citation", func(t *testing.T) {
		// Syntactically valid JSON, but the citation is fabricated.
		env := testEnvelope(t)
		scope := testScope(env)
		rep := testReport(env)
		rep.Hypotheses[0].EvidenceIDs = []string{"ev-doc-01", "ev-fabricated-999"}
		rep.Findings[0].EvidenceIDs = []string{"ev-doc-01", "ev-fabricated-999"}
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(submitBytes(t, rep))}}
		ex := successExecutor()
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if !errors.Is(err, ErrGrounding) {
			t.Fatalf("err = %v, want ErrGrounding", err)
		}
		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
			t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
		}
		if out.Report != nil {
			t.Fatal("fabricated report accepted")
		}
		if ex.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0", ex.Calls())
		}
		t.Logf("APA55-C semantically_invalid_citation rejected at grounding (ErrGrounding), zero tool calls")
	})
}

// ---------------------------------------------------------------------------
// Group B: tool use — deterministic gates through the real Loop
// ---------------------------------------------------------------------------

// TestAPA55_Boundary_ToolUse (was TestAPA55_B_ToolUse) runs the tool-use
// adversarial cases. Deterministic cases use FakeModelClient through the real
// Loop; the control and the empty-result case are single honest measurements.
func TestAPA55_Boundary_ToolUse(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)

	t.Run("valid_tool_call_control", func(t *testing.T) {
		fake := &FakeModelClient{Responses: []ModelResponse{
			modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
			modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
		}}
		ex := successExecutor()
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if out.Outcome != OutcomeReportReady {
			t.Fatalf("Outcome = %q, want REPORT_READY", out.Outcome)
		}
		if ex.Calls() != 1 {
			t.Fatalf("executor Calls() = %d, want 1", ex.Calls())
		}
		t.Logf("APA55-B valid_tool_call_control: REPORT_READY, 1 tool call")
	})

	t.Run("nonexistent_tool", func(t *testing.T) {
		req, err := investigate.NewRequest(invest.ToolGetClaim, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, 1)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Tool = "get_nonexistent"
		raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: "get_nonexistent", Request: &req})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw), modelResp(raw)}}
		ex := successExecutor()
		_, err = newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if err == nil {
			t.Fatal("nonexistent tool accepted")
		}
		if ex.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0: nonexistent tool must not execute", ex.Calls())
		}
		t.Logf("APA55-B nonexistent_tool rejected: %v, zero tool calls", err)
	})

	t.Run("unauthorized_tool", func(t *testing.T) {
		// ToolGetEvidence is allowed, but ToolSearchEvidence is not in scope.AllowTools.
		req, err := investigate.NewRequest(invest.ToolSearchEvidence, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, 1)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolSearchEvidence, Request: &req})
		if err != nil {
			// NewRequest may reject the tool if not allowlisted; that is also a rejection.
			t.Logf("APA55-B unauthorized_tool rejected at NewRequest: %v", err)
			return
		}
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw), modelResp(raw)}}
		ex := successExecutor()
		_, err = newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if err == nil {
			t.Fatal("unauthorized tool accepted")
		}
		if ex.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0: unauthorized tool must not execute", ex.Calls())
		}
		t.Logf("APA55-B unauthorized_tool rejected: %v, zero tool calls", err)
	})

	t.Run("wrong_knob_subject_id_on_get_evidence", func(t *testing.T) {
		req, err := investigate.NewRequest(invest.ToolGetEvidence, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, 1)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.SubjectID = "subj-123"
		raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolGetEvidence, Request: &req})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw), modelResp(raw)}}
		ex := successExecutor()
		_, err = newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if err == nil {
			t.Fatal("wrong knob accepted")
		}
		if ex.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0: wrong knob must not execute", ex.Calls())
		}
		t.Logf("APA55-B wrong_knob rejected: %v, zero tool calls", err)
	})

	t.Run("missing_required_knob", func(t *testing.T) {
		// A request with blank tenant_id (required identity echo).
		req, err := investigate.NewRequest(invest.ToolGetEvidence, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, 1)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.TenantID = ""
		raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolGetEvidence, Request: &req})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw), modelResp(raw)}}
		ex := successExecutor()
		_, err = newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if err == nil {
			t.Fatal("missing required knob accepted")
		}
		if ex.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0", ex.Calls())
		}
		t.Logf("APA55-B missing_required_knob rejected: %v, zero tool calls", err)
	})

	t.Run("extra_unknown_knob", func(t *testing.T) {
		// A request with an unknown field spliced in.
		req, err := investigate.NewRequest(invest.ToolGetEvidence, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, 1)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolGetEvidence, Request: &req})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		bad := withUnknownField(t, raw, `"bogus_knob":"x"`)
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(bad), modelResp(bad)}}
		ex := successExecutor()
		_, err = newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if err == nil {
			t.Fatal("extra unknown knob accepted")
		}
		if ex.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0", ex.Calls())
		}
		t.Logf("APA55-B extra_unknown_knob rejected: %v, zero tool calls", err)
	})

	t.Run("repeated_identical_call", func(t *testing.T) {
		fake := &FakeModelClient{Responses: []ModelResponse{
			modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
			modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		}}
		ex := successExecutor()
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if err == nil {
			t.Fatal("repeated identical call accepted")
		}
		if out.EscalationReason != EscalationRepetition {
			t.Fatalf("Reason = %q, want EscalationRepetition", out.EscalationReason)
		}
		if ex.Calls() != 1 {
			t.Fatalf("executor Calls() = %d, want 1: repetition must not duplicate effect", ex.Calls())
		}
		t.Logf("APA55-B repeated_identical_call escalated REPETITION, 1 tool call (no duplicate effect)")
	})

	t.Run("empty_result", func(t *testing.T) {
		ex := investigate.NewExecutor(map[invest.ToolName]investigate.ToolFunc{
			invest.ToolGetEvidence: func(_ context.Context, req investigate.Request) (investigate.Response, error) {
				return investigate.Response{Tool: req.Tool, RowCount: 0, IDs: []string{}}, nil
			},
		}, time.Time{})
		fake := &FakeModelClient{Responses: []ModelResponse{
			modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 1)),
			modelResp(submitBytes(t, testReport(env))),
		}}
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		// Empty result is deterministic handling: the loop continues, no crash.
		t.Logf("APA55-B empty_result handled: Outcome=%q", out.Outcome)
	})

	t.Run("tool_timeout_failure", func(t *testing.T) {
		ex := failingExecutor()
		fake := &FakeModelClient{Responses: []ModelResponse{
			modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 1)),
			modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 1)),
		}}
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if err == nil {
			t.Fatal("tool failure accepted")
		}
		t.Logf("APA55-B tool_timeout_failure bounded: %v, Outcome=%q", err, out.Outcome)
	})
}

// TestAPA55_Boundary_EvidenceRefs preserves the two deterministic evidence-
// reference cases that lived in TestAPA55_A_Evidence in the untracked artifact.
//
// That test was classified as "live, superseded" as a whole, which was wrong:
// alongside its (correctly superseded) live subtests it carried two
// deterministic ones. nonexistent_evidence_id_in_trigger in particular is
// genuine grounding-boundary coverage — an unknown evidence ID referenced from a
// trigger must be rejected terminally with ZERO tool execution — and existed
// nowhere else. The full t.Run inventory of the artifact is reconciled in the
// PR description; this function closes the gap.
func TestAPA55_Boundary_EvidenceRefs(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)

	// Was: TestAPA55_A_Evidence/nonexistent_evidence_id_in_trigger.
	// STRONG: three assertions, preserved verbatim.
	t.Run("nonexistent_evidence_id_in_trigger", func(t *testing.T) {
		rep := testReport(env)
		rep.Hypotheses[0].EvidenceIDs = []string{"ev-doc-01", "ev-nonexistent-999"}
		rep.Findings[0].EvidenceIDs = []string{"ev-doc-01", "ev-nonexistent-999"}
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(submitBytes(t, rep))}}
		ex := successExecutor()
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if !errors.Is(err, ErrGrounding) {
			t.Fatalf("err = %v, want ErrGrounding", err)
		}
		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
			t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
		}
		if ex.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0", ex.Calls())
		}
		t.Logf("APA55-A nonexistent_evidence_id_in_trigger rejected at grounding (ErrGrounding), zero tool calls")
	})

	// Was: TestAPA55_A_Evidence/duplicate_cited_evidence.
	// CHARACTERIZATION ONLY — it logs and asserts nothing. Preserved faithfully
	// rather than quietly dropped or quietly strengthened; strengthening is
	// tracked in Linear APA-84 and must not widen this preservation change.
	t.Run("duplicate_cited_evidence", func(t *testing.T) {
		rep := testReport(env)
		rep.Hypotheses[0].EvidenceIDs = []string{"ev-doc-01", "ev-doc-01"}
		rep.Findings[0].EvidenceIDs = []string{"ev-doc-01", "ev-doc-01"}
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(submitBytes(t, rep))}}
		ex := successExecutor()
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		// Duplicate-cited evidence: deterministic handling. The boundary
		// either accepts (grounded, since ev-doc-01 is known) or rejects;
		// it must not crash or leak.
		t.Logf("APA55-A duplicate_cited_evidence handled: Outcome=%q err=%v", out.Outcome, err)
	})
}
